package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"project-alpha/internal/assistant"
	"project-alpha/internal/platform"
)

func TestAssistantRoutesAndToolBoundary(t *testing.T) {
	f := setup(t)
	requireStatus(t, f.request(t, "GET", "/api/assistant/health", nil), 200)
	overview := f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, overview, 200)
	if !strings.Contains(overview.Body.String(), "assistant_health") {
		t.Fatal("missing control health")
	}
	requireStatus(t, f.request(t, "GET", "/api/assistant/sessions", nil), 200)
	requireStatus(t, f.request(t, "GET", "/api/assistant/memories", nil), 200)
	requireStatus(t, f.request(t, "POST", "/api/assistant/sessions", map[string]string{"message": "hello"}), 409)
	var method, path string
	node, server := worker(t, strings.Repeat("a", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		method = r.Method
		path = r.URL.Path
		return 200, map[string]any{"managed": []any{}}, nil
	}))
	add(t, f, node, server, "worker")
	value, err := f.control.assistantTool(context.Background(), f.user, assistant.ToolCall{Name: "list_nodes"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), node.Token) {
		t.Fatal("node token exposed to model")
	}
	_, err = f.control.assistantTool(context.Background(), f.user, assistant.ToolCall{Name: "get_node_containers", NodeID: node.ID})
	if err != nil || method != "GET" || path != "/api/containers" {
		t.Fatal(method, path, err)
	}
	for _, call := range []assistant.ToolCall{{Name: "restart", NodeID: node.ID}, {Name: "get_node_containers", NodeID: "../../etc"}, {Name: "https://example.com"}, {Name: "list_nodes", NodeID: node.ID}} {
		if _, err = f.control.assistantTool(context.Background(), f.user, call); err == nil {
			t.Fatal("invalid tool accepted", call)
		}
	}
	saved := f.csrf
	f.csrf = "invalid"
	requireStatus(t, f.request(t, "DELETE", "/api/assistant/memories", nil), 403)
	f.csrf = saved
	if _, err = f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "GET", "/api/assistant/sessions", nil), 403)
	requireStatus(t, f.request(t, "GET", "/api/assistant/health", nil), 403)
}
