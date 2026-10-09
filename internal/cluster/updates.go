package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/updates"
)

// updateDispatch is intentionally outside all business-protocol gates.
func (h *Control) updateDispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "更新设置需要管理员权限")
	}
	if h.Updates == nil {
		return 0, nil, httpapi.NewError(503, "更新服务未就绪")
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/updates/")
	if rest == "github-token" {
		switch r.Method {
		case "GET":
			return 200, h.Updates.GitHubTokenSettings(), nil
		case "PUT":
			var input struct {
				Revision int    `json:"revision"`
				Token    string `json:"token"`
				Clear    bool   `json:"clear"`
			}
			if err := httpapi.DecodeBody(w, r, &input); err != nil {
				return 0, nil, err
			}
			h.updateTokenMu.Lock()
			defer h.updateTokenMu.Unlock()
			if err := h.Updates.SaveGitHubToken(input.Revision, input.Token, input.Clear); err != nil {
				return 0, nil, err
			}
			failed := h.pushGitHubTokens(r.Context())
			return 200, map[string]any{"settings": h.Updates.GitHubTokenSettings(), "pending_nodes": failed}, nil
		default:
			return 0, nil, httpapi.NewError(405, "仅支持 GET 和 PUT")
		}
	}
	if rest == "targets" && r.Method == "GET" {
		nodes, err := h.nodes("")
		return 200, map[string]any{"nodes": nodes}, err
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || (parts[1] != "settings" && parts[1] != "health" && parts[1] != "update") {
		return 0, nil, httpapi.NewError(404, "更新接口不存在")
	}
	clone := r.Clone(r.Context())
	clone.URL.Path = updates.Path + "/" + parts[1]
	if parts[0] == "control" {
		return h.Updates.Dispatch(w, clone)
	}
	n, err := h.node(parts[0])
	if err != nil {
		return 0, nil, err
	}
	if parts[1] == "update" && r.Method == "POST" {
		if err := h.syncGitHubToken(r.Context(), n); err != nil {
			return 0, nil, err
		}
	}
	var body any
	if r.Method == "PUT" {
		if err := httpapi.DecodeBody(w, r, &body); err != nil {
			return 0, nil, err
		}
	}
	var result json.RawMessage
	if err := h.call(r.Context(), n, r.Method, clone.URL.Path, body, user, &result); err != nil {
		return 0, nil, err
	}
	status := 200
	if parts[1] == "update" && r.Method == "POST" {
		status = 202
	}
	return status, result, nil
}

func (h *Control) pushGitHubToken(ctx context.Context, n Node, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out json.RawMessage
	err := h.call(ctx, n, "PUT", updates.Path+"/github-token", map[string]string{"token": token}, platform.User{ID: h.identity, Username: "update-control", Role: "admin"}, &out)
	if err != nil {
		return httpapi.NewError(502, "GitHub Token 下发失败，请检查节点连接与服务版本")
	}
	return nil
}

func (h *Control) syncGitHubToken(ctx context.Context, n Node) error {
	h.updateTokenMu.Lock()
	defer h.updateTokenMu.Unlock()
	return h.pushGitHubToken(ctx, n, h.Updates.GitHubToken())
}

// The caller holds updateTokenMu so rotations cannot overtake earlier delivery.
func (h *Control) pushGitHubTokens(ctx context.Context) []string {
	nodes, err := h.nodes("")
	if err != nil {
		return []string{"无法读取节点列表"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token := h.Updates.GitHubToken()
	failed := make([]bool, len(nodes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, n := range nodes {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				failed[i] = true
				return
			}
			failed[i] = h.pushGitHubToken(ctx, n, token) != nil
		})
	}
	wg.Wait()
	pending := []string{}
	for i, n := range nodes {
		if failed[i] {
			pending = append(pending, n.ID)
		}
	}
	return pending
}

func (h *Control) syncGitHubTokens(ctx context.Context) {
	h.updateTokenMu.Lock()
	defer h.updateTokenMu.Unlock()
	h.pushGitHubTokens(ctx)
}

func (h *Control) releaseNotice(ctx context.Context, raw json.RawMessage) (any, error) {
	if h.Updates == nil {
		return nil, httpapi.NewError(503, "更新服务未就绪")
	}
	var notice updates.Release
	if err := json.Unmarshal(raw, &notice); err != nil {
		return nil, httpapi.NewError(400, "release 通知无效")
	}
	if err := h.Updates.Receive(notice); err != nil {
		return nil, err
	}
	// Registry keeps its outbox pending until every registered worker has durably
	// received the notice. Each worker deduplicates it, including across restart.
	nodes, err := h.nodes("worker")
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	slots := make(chan struct{}, 8)
	for _, n := range nodes {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				mu.Lock()
				first = ctx.Err()
				mu.Unlock()
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := h.syncGitHubToken(callCtx, n); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				return
			}
			var out json.RawMessage
			e := h.call(callCtx, n, "POST", updates.Path+"/release", notice, platform.User{ID: h.identity, Username: "release-webhook", Role: "admin"}, &out)
			if e != nil {
				mu.Lock()
				if first == nil {
					first = e
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err = h.Updates.Forwarded(notice); err != nil {
		return nil, err
	}
	if err = h.Updates.Automatic(); err != nil {
		return nil, err
	}
	return map[string]bool{"accepted": true}, nil
}
