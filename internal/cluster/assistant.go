package cluster

import (
	"context"
	"time"

	"project-alpha/internal/assistant"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Tools select fixed, read-only worker APIs. Neither URLs, methods, paths nor
// credentials are accepted from the model. Node credentials stay in Go.
func (h *Control) assistantTool(ctx context.Context, user platform.User, call assistant.ToolCall) (any, error) {
	if user.Role == "member" {
		return h.memberAssistantTool(ctx, user, call)
	}
	if user.Role != "admin" {
		return nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if call.Name == "list_nodes" && call.NodeID == "" {
		nodes, err := h.nodes("")
		return map[string]any{"nodes": nodes, "checked_at": platform.Now()}, err
	}
	paths := map[string]string{
		"get_node_inventory":  "/api/worker/inventory",
		"get_node_storage":    "/api/state",
		"get_node_containers": "/api/containers",
		"get_node_processes":  "/api/process/forest",
		"get_node_gpu":        "/api/gpu/overview",
	}
	path, ok := paths[call.Name]
	if !ok || !identifier.MatchString(call.NodeID) {
		return nil, httpapi.NewError(400, "未知运维工具或节点标识")
	}
	node, err := h.node(call.NodeID)
	if err != nil {
		return nil, err
	}
	if node.Kind != "worker" {
		return nil, httpapi.NewError(400, "此工具仅支持计算节点")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var value any
	if err = h.call(ctx, node, "GET", path, nil, user, &value); err != nil {
		return nil, err
	}
	return map[string]any{"node_id": node.ID, "name": node.Name, "checked_at": platform.Now(), "data": value}, nil
}
