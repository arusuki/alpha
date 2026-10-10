package cluster

import (
	"context"
	"encoding/json"
	"log"
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
	h.updateTokenMu.RLock()
	defer h.updateTokenMu.RUnlock()
	return h.pushGitHubToken(ctx, n, h.Updates.GitHubToken())
}

// The caller holds updateTokenMu (shared for delivery, exclusive for rotation).
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
	nodes, err := h.nodes("worker")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if err := h.Updates.QueueDeliveries(notice, ids); err != nil {
		return nil, err
	}
	if err := h.Updates.Forwarded(notice); err != nil {
		return nil, err
	}
	// The durable per-node queue owns retries. Neither an offline worker nor a
	// local preparation failure can prevent registry from acknowledging delivery.
	h.wakeReleaseDelivery()
	if err := h.Updates.Automatic(); err != nil {
		log.Printf("control update: %v", err)
	}
	return map[string]bool{"accepted": true}, nil
}

func (h *Control) syncUpdateNode(ctx context.Context, n Node) {
	// Independent nodes can deliver concurrently, while token rotations wait for
	// in-flight sends and prevent any old credential from arriving afterwards.
	h.updateTokenMu.RLock()
	defer h.updateTokenMu.RUnlock()
	token := h.Updates.GitHubToken()
	if err := h.pushGitHubToken(ctx, n, token); err != nil {
		return
	}
	for _, notice := range h.Updates.PendingForNode(n.ID) {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var out json.RawMessage
		err := h.call(callCtx, n, "POST", updates.Path+"/release", notice, platform.User{ID: h.identity, Username: "release-webhook", Role: "admin"}, &out)
		cancel()
		if err != nil {
			return
		}
		if err := h.Updates.DeliveredToNode(n.ID, notice); err != nil {
			log.Printf("persist worker release delivery: %v", err)
			return
		}
	}
}

func (h *Control) syncUpdateNodes(ctx context.Context, nodes []Node) {
	// Each node has its own timeout. A batch deadline would repeatedly starve
	// later nodes when earlier slots are occupied by offline workers.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, n := range nodes {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			h.syncUpdateNode(ctx, n)
		})
	}
	wg.Wait()
}

func (h *Control) syncUpdateDeliveries(ctx context.Context) {
	if h.Updates == nil {
		return
	}
	nodes, err := h.nodes("")
	if err != nil {
		log.Printf("read release delivery targets: %v", err)
		return
	}
	h.syncUpdateNodes(ctx, nodes)
}

// Delivery work is woken by durable receipts and the existing registry
// heartbeat. It has no independent polling or health-check timer.
func (h *Control) initReleaseDelivery() {
	ctx, cancel := context.WithCancel(context.Background())
	h.releaseCancel = cancel
	h.releaseWake = make(chan struct{}, 1)
	h.releaseDone = make(chan struct{})
	go func() {
		defer close(h.releaseDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.releaseWake:
				h.syncUpdateDeliveries(ctx)
			}
		}
	}()
	h.wakeReleaseDelivery()
}
func (h *Control) wakeReleaseDelivery() {
	select {
	case h.releaseWake <- struct{}{}:
	default:
	}
}
