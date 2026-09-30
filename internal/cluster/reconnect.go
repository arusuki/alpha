package cluster

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (h *Control) reconnectNode(r *http.Request, user platform.User, id string) (int, any, error) {
	h.nodeMu.Lock()
	defer h.nodeMu.Unlock()
	if h.nodesClosed {
		return 0, nil, httpapi.NewError(503, "总控正在关闭")
	}
	// Re-read under the mutation lock so a removed or edited registry cannot be
	// restarted with stale configuration.
	n, err := h.node(id)
	if err != nil {
		return 0, nil, err
	}
	if err := h.DB.Transaction(func(tx *sql.Tx) error {
		return platform.Audit(tx, user.Username, "cluster.node.reconnect", n.Name+" / "+n.ID)
	}); err != nil {
		return 0, nil, err
	}
	if n.Kind == "registry" {
		// Replacing the link cancels the current retry delay and starts dialing now.
		h.startRegistry(n)
		return http.StatusAccepted, map[string]bool{"ok": true}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var inventory Inventory
	if err := h.call(ctx, n, "GET", "/api/worker/inventory", nil, user, &inventory); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]bool{"ok": true}, nil
}
