package cluster

import (
	"context"
	"net/http"
	"sync"
	"time"

	"project-alpha/internal/containers"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

type memberPasswordNodeResult struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	containers.PasswordResetResult
}

func (h *Control) resetMemberPassword(w http.ResponseWriter, r *http.Request, id string, user platform.User) (int, any, error) {
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "不支持该方法")
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	gate := h.memberGate(id)
	if !gate.TryLock() {
		return 0, nil, httpapi.NewError(409, "使用者资源操作正在执行，请稍后重置密码")
	}
	defer gate.Unlock()
	nodes, err := h.nodes("worker")
	if err != nil {
		return 0, nil, err
	}
	store := &members.Store{Database: h.DB}
	// Save first: sessions are revoked atomically and future provisioning uses the
	// new password. Existing container failures are returned explicitly for retry.
	if err := store.ResetPassword(id, req.Password, user.Username); err != nil {
		return 0, nil, err
	}
	var username string
	if err := h.DB.SQL.QueryRow("SELECT username FROM members WHERE id=?", id).Scan(&username); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	results := make([]memberPasswordNodeResult, len(nodes))
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Go(func() {
			result := memberPasswordNodeResult{NodeID: node.ID, NodeName: node.Name, PasswordResetResult: containers.PasswordResetResult{Errors: []string{}}}
			defer func() { results[i] = result }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result.Errors = append(result.Errors, "节点密码重置超时，结果未确认，请使用相同密码重试")
				return
			}
			err := h.call(ctx, node, "POST", "/api/containers/members/"+id+"/password", map[string]string{"username": username, "password": req.Password}, user, &result.PasswordResetResult)
			if err != nil {
				result.OK = false
				result.Errors = append(result.Errors, err.Error())
			} else if !result.OK && len(result.Errors) == 0 {
				result.Errors = append(result.Errors, "节点未确认密码重置完成，请重试")
			}
		})
	}
	wg.Wait()
	ok, updated := true, 0
	for _, result := range results {
		ok = ok && result.OK
		updated += result.Updated
	}
	return 200, map[string]any{"ok": ok, "account_reset": true, "updated": updated, "nodes": results}, nil
}
