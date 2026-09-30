package cluster

import (
	"context"
	"database/sql"
	"net/http"
	"sync"

	"project-alpha/internal/agent"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (h *Control) nodeGate(id string) *sync.Mutex {
	h.agentMu.Lock()
	defer h.agentMu.Unlock()
	if h.gates[id] == nil {
		h.gates[id] = &sync.Mutex{}
	}
	return h.gates[id]
}

func (h *Control) agentUser(nodeID, userID string) (platform.User, error) {
	var user platform.User
	err := h.DB.SQL.QueryRow("SELECT id,username,role FROM users WHERE id=? AND enabled=1 AND role='admin'", userID).Scan(&user.ID, &user.Username, &user.Role)
	if err == sql.ErrNoRows {
		return user, httpapi.NewError(403, "Agent 分析需要有效的总控管理员权限")
	}
	if err == nil {
		_, err = h.node(nodeID)
	}
	return user, err
}

func (h *Control) dispatchAgent(w http.ResponseWriter, r *http.Request, user platform.User, node Node, path string) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "Agent 分析需要管理员权限")
	}
	if path == "/api/agent/settings" {
		return 0, nil, httpapi.NewError(404, "请在总控配置 Agent")
	}
	// Serialize admission and record deletion on this node. Streams remain independent.
	if r.Method != "GET" {
		gate := h.nodeGate(node.ID)
		gate.Lock()
		defer gate.Unlock()
		var busy int
		if err := h.DB.SQL.QueryRow("SELECT count(*) FROM agent_sessions WHERE node_id=? AND user_id<>? AND status IN ('queued','scanning','running','cancelling')", node.ID, user.ID).Scan(&busy); err != nil {
			return 0, nil, err
		}
		if busy > 0 {
			return 0, nil, httpapi.NewError(409, "此节点已有其他管理员的分析正在执行")
		}
	}
	h.agentMu.Lock()
	if h.closed {
		h.agentMu.Unlock()
		return 0, nil, httpapi.NewError(503, "总控正在关闭")
	}
	key := node.ID + ":" + user.ID
	handler := h.agents[key]
	if handler == nil {
		store := &agent.Store{Database: h.DB, NodeID: node.ID}
		records := &remoteRecords{control: h, nodeID: node.ID, userID: user.ID}
		manager, err := agent.NewManager(store, records, func(id string) error {
			_, err := h.agentUser(node.ID, id)
			return err
		})
		if err != nil {
			h.agentMu.Unlock()
			return 0, nil, err
		}
		handler = agent.NewHandler(store, manager)
		h.agents[key] = handler
	}
	h.agentMu.Unlock()
	request := r.Clone(r.Context())
	request.URL.Path = path
	return handler.Dispatch(w, request, user)
}

func (h *Control) deleteRecord(r *http.Request, user platform.User, node Node, id string) (int, any, error) {
	gate := h.nodeGate(node.ID)
	gate.Lock()
	defer gate.Unlock()
	// An active analysis can acquire its snapshot asynchronously, so protect the
	// entire node while it runs, including directory tasks created by its tools.
	var busy int
	if err := h.DB.SQL.QueryRow("SELECT count(*) FROM agent_sessions WHERE node_id=? AND status IN ('queued','scanning','running','cancelling')", node.ID).Scan(&busy); err != nil {
		return 0, nil, err
	}
	if busy > 0 {
		return 0, nil, httpapi.NewError(409, "该节点正在分析，请结束相关分析后再删除扫描记录")
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteTimeout)
	defer cancel()
	var result struct {
		DeletedIDs     []string `json:"deleted_ids"`
		CleanupPending bool     `json:"cleanup_pending"`
	}
	if err := h.call(ctx, node, "DELETE", "/api/jobs/"+id, nil, user, &result); err != nil {
		return 0, nil, err
	}
	err := (&agent.Store{Database: h.DB, NodeID: node.ID}).ForgetRecords(result.DeletedIDs)
	return 200, result, err
}
