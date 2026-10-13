package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"project-alpha/internal/assistant"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMemberAssistantRoutesAndTokenBoundary(t *testing.T) {
	f := setup(t)
	id, resource := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	path := "/api/member-assistant"
	for _, route := range []string{"/me", "/sessions", "/memories"} {
		requireStatus(t, selfCall(f, "GET", path+route, "", nil), 401)
		requireStatus(t, selfCall(f, "GET", path+route, resource, nil), 401)
		requireStatus(t, f.request(t, "GET", path+route, nil), 401)
		requireStatus(t, selfCall(f, "GET", path+route, token, nil), 200)
	}
	requireStatus(t, selfCall(f, "POST", path+"/sessions", token, map[string]string{"message": "hello"}), 409) // model not configured
	requireStatus(t, selfCall(f, "GET", path+"/settings", token, nil), 404)
	requireStatus(t, selfCall(f, "POST", path+"/sessions", token, map[string]string{"message": "hello", "user_id": "other"}), 400)
	for _, route := range []string{"/api/assistant/settings", "/api/assistant/sessions", "/api/cluster/nodes"} {
		requireStatus(t, selfCall(f, "GET", route, token, nil), 401)
	}
	r := httptest.NewRequest("POST", path+"/sessions", strings.NewReader(`{"message":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	requireStatus(t, w, 403)
	requireStatus(t, selfCall(f, "POST", "/api/status/alice/password", token, map[string]string{"current_password": "Member-password-123", "password": "Changed-password-123"}), 200)
	requireStatus(t, selfCall(f, "GET", path+"/sessions", token, nil), 401)
	token = statusLogin(t, f, "alice", "Changed-password-123")
	requireStatus(t, selfCall(f, "POST", path+"/logout", token, map[string]any{}), 200)
	requireStatus(t, selfCall(f, "GET", path+"/sessions", token, nil), 401)
}

func TestMemberAssistantToolsFilterBeforeModel(t *testing.T) {
	f := setup(t)
	id, _ := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	own, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	inventory := Inventory{Host: "worker", Active: map[string]string{"secret": "scan-secret"}, Containers: []Container{{ID: own, Name: "own", Owner: "alice"}, {ID: other, Name: "private-container", Owner: "bob"}}}
	paths := map[string]int{}
	node, server := worker(t, strings.Repeat("c", 32), inventory, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if r.Method != "GET" || u.ID != id || u.Username != "member:alice" || u.Role != "viewer" {
			t.Error("wrong worker identity", u, r.Method)
		}
		paths[r.URL.Path]++
		switch r.URL.Path {
		case "/api/process/forest":
			return 200, map[string]any{"forest": map[string]any{"containers": []map[string]any{{"id": "docker://" + own[:15], "name": "own-process"}, {"id": other, "name": "private-process"}}, "host": map[string]string{"id": "host", "name": "host-secret"}}}, nil
		case "/api/member-disk":
			return 200, map[string]string{"scope": "member-disk"}, nil
		case "/api/gpu/overview":
			return 200, map[string]string{"scope": "shared-gpu"}, nil
		}
		return 0, nil, httpapi.NewError(404, "unexpected")
	}))
	add(t, f, node, server, "node")
	user := platform.User{ID: id, Username: "member:alice", Role: "member"}
	for _, name := range []string{"list_nodes", "get_node_inventory", "get_node_containers", "get_node_processes", "get_node_storage", "get_node_gpu"} {
		call := assistant.ToolCall{Name: name, NodeID: node.ID}
		if name == "list_nodes" {
			call.NodeID = ""
		}
		value, err := f.control.assistantTool(context.Background(), user, call)
		if err != nil {
			t.Fatal(name, err)
		}
		raw, _ := json.Marshal(value)
		for _, secret := range []string{other, "private-container", "private-process", "host-secret", "scan-secret", node.Token, server.URL} {
			if strings.Contains(string(raw), secret) {
				t.Fatal(name, "leaked", secret)
			}
		}
		if name == "get_node_processes" && !strings.Contains(string(raw), "own-process") {
			t.Fatal("own process missing", string(raw))
		}
	}
	if paths["/api/member-disk"] != 1 || paths["/api/state"] != 0 || paths["/api/containers"] != 0 {
		t.Fatal("unsafe member endpoint", paths)
	}
	if _, err := f.control.assistantTool(context.Background(), user, assistant.ToolCall{Name: "shell", NodeID: node.ID}); err == nil {
		t.Fatal("unknown tool allowed")
	}
	ambiguous := []Container{{ID: strings.Repeat("c", 64), Owner: "alice"}, {ID: strings.Repeat("c", 63) + "d", Owner: "bob"}}
	if memberOwnsProcess(strings.Repeat("c", 12), ambiguous, "alice") {
		t.Fatal("ambiguous process exposed")
	}
}
