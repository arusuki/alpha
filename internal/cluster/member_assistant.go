package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"project-alpha/internal/assistant"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
)

func (h *Control) memberAssistant(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if !strings.HasPrefix(r.URL.Path, "/api/member-assistant/") {
		return 0, nil, nil
	}
	if !httpapi.MemberAssistantAllowed(r) {
		return 0, nil, httpapi.NewError(404, "使用者助理接口不存在")
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return 0, nil, httpapi.NewError(401, "请使用注册时设置的密码登录")
	}
	if r.URL.Path == "/api/member-assistant/me" {
		u, err := members.SessionUser(h.DB.SQL, token)
		return 200, map[string]string{"id": u.ID, "username": strings.TrimPrefix(u.Username, "member:")}, err
	}
	if r.URL.Path == "/api/member-assistant/logout" {
		store := &members.Store{Database: h.DB}
		if _, err := store.SessionID(token); err != nil {
			return 0, nil, err
		}
		return 200, map[string]bool{"ok": true}, store.Logout(token)
	}
	copy := r.Clone(r.Context())
	copy.URL.Path = strings.Replace(r.URL.Path, "/api/member-assistant/", "/api/assistant/", 1)
	return h.Assistant.DispatchMember(w, copy, token)
}

// Members share the status page's cluster capacity views; private container
// details and process arguments are filtered before they can reach the model.
func (h *Control) memberAssistantTool(ctx context.Context, user platform.User, call assistant.ToolCall) (any, error) {
	if call.Name == "list_nodes" && call.NodeID == "" {
		nodes, err := h.nodes("worker")
		out := []map[string]string{}
		for _, n := range nodes {
			out = append(out, map[string]string{"id": n.ID, "name": n.Name})
		}
		return map[string]any{"nodes": out, "checked_at": platform.Now()}, err
	}
	if !identifier.MatchString(call.NodeID) {
		return nil, httpapi.NewError(400, "无效的节点标识")
	}
	switch call.Name {
	case "get_node_inventory", "get_node_containers", "get_node_processes", "get_node_storage", "get_node_gpu":
	default:
		return nil, httpapi.NewError(403, "使用者无权调用此工具")
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
	// Worker authentication stays on the existing trusted control channel.
	user.Role = "viewer"
	var value any
	if call.Name == "get_node_storage" || call.Name == "get_node_gpu" {
		path := "/api/member-disk"
		if call.Name == "get_node_gpu" {
			path = "/api/gpu/overview"
		}
		if err := h.call(ctx, node, "GET", path, nil, user, &value); err != nil {
			return nil, err
		}
	} else {
		var inventory Inventory
		if err := h.call(ctx, node, "GET", "/api/worker/inventory", nil, user, &inventory); err != nil {
			return nil, err
		}
		owned := []Container{}
		for _, c := range inventory.Containers {
			if c.Owner == strings.TrimPrefix(user.Username, "member:") {
				owned = append(owned, c)
			}
		}
		value = map[string]any{"host": inventory.Host, "containers": owned, "observed_at": inventory.ObservedAt, "scanning": inventory.Active != nil}
		if call.Name == "get_node_processes" {
			var view struct {
				Status json.RawMessage `json:"status"`
				Forest process.Forest  `json:"forest"`
			}
			if err := h.call(ctx, node, "GET", "/api/process/forest", nil, user, &view); err != nil {
				return nil, err
			}
			filtered := []*process.Container{}
			for _, c := range view.Forest.Containers {
				if c != nil && memberOwnsProcess(c.ID, inventory.Containers, strings.TrimPrefix(user.Username, "member:")) {
					filtered = append(filtered, c)
				}
			}
			view.Forest.Containers, view.Forest.Host = filtered, nil
			value = view
		}
	}
	return map[string]any{"node_id": node.ID, "name": node.Name, "checked_at": platform.Now(), "data": value}, nil
}

// Resolve Tetragon's runtime-prefixed/truncated IDs only when unambiguous across
// the entire inventory, including other members' containers.
func memberOwnsProcess(id string, containers []Container, owner string) bool {
	if _, value, ok := strings.Cut(id, "://"); ok {
		id = value
	}
	if len(id) < 12 || len(id) > 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return false
	}
	matches, owned := 0, false
	for _, c := range containers {
		if strings.HasPrefix(c.ID, id) {
			matches++
			owned = c.Owner == owner
		}
	}
	return matches == 1 && owned
}
