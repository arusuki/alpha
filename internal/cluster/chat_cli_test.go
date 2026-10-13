package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"project-alpha/internal/platform"
)

func TestChatCLIAutomaticDispatchConfigurationAndRetry(t *testing.T) {
	f := setup(t)
	id, _ := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	response := f.request(t, "GET", "/api/assistant/cli", nil)
	requireStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "/usr/local/bin/alpha-chat") {
		t.Fatal(response.Body.String())
	}
	cfg := map[string]any{"revision": 1, "server": "https://control.test", "command_path": "/opt/ops/bin/ask"}
	requireStatus(t, f.request(t, "PUT", "/api/assistant/cli", cfg), 202)
	requireStatus(t, f.request(t, "PUT", "/api/assistant/cli", cfg), 409)
	var mu sync.Mutex
	var requests []map[string]string
	fail := false
	node, server := worker(t, strings.Repeat("d", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if r.Method != "PUT" || r.URL.Path != "/api/containers/members/"+id+"/cli" || u.ID != id || u.Role != "admin" {
			t.Errorf("unexpected dispatch %s %s %+v", r.Method, r.URL.Path, u)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, body)
		if fail {
			return 200, map[string]any{"ok": false, "errors": []string{"容器停止"}}, nil
		}
		return 200, map[string]any{"ok": true, "installed": 1, "errors": []string{}}, nil
	}))
	add(t, f, node, server, "Auto CLI")
	awaitIdle(t, f, id)
	mu.Lock()
	got := requests[0]
	n := len(requests)
	mu.Unlock()
	if n != 1 || len(got) != 3 || got["username"] != "alice" || got["command_path"] != "/opt/ops/bin/ask" || got["server"] != "https://control.test" {
		t.Fatal(requests)
	}
	if _, err := f.db.SQL.Exec(`INSERT INTO bastion_tailscale VALUES('share','Share',1,'100.64.0.3',2222,9765,'http://100.100.0.2:8765'); UPDATE member_access SET tailscale_id='share' WHERE member_id=?`, id); err != nil {
		t.Fatal(err)
	}
	cfg["revision"], cfg["server"], cfg["command_path"] = 2, "", "/usr/local/bin/alpha-chat"
	mu.Lock()
	fail = true
	mu.Unlock()
	requireStatus(t, f.request(t, "PUT", "/api/assistant/cli", cfg), 202)
	awaitIdle(t, f, id)
	response = f.request(t, "GET", "/api/assistant/cli", nil)
	requireStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "failed") || !strings.Contains(response.Body.String(), "容器停止") {
		t.Fatal(response.Body.String())
	}
	mu.Lock()
	fail = false
	got = requests[len(requests)-1]
	mu.Unlock()
	if got["server"] != "http://100.64.0.3:9765" {
		t.Fatal("wrong automatic entrance", got)
	}
	requireStatus(t, f.request(t, "POST", "/api/assistant/cli/sync", map[string]any{}), 202)
	awaitIdle(t, f, id)
	response = f.request(t, "GET", "/api/assistant/cli", nil)
	if !strings.Contains(response.Body.String(), "ready") {
		t.Fatal(response.Body.String())
	}
	_, _, err := f.control.dispatchChatCLI(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/assistant/cli", nil), platform.User{Role: "member"})
	if err == nil {
		t.Fatal("member accessed deployment settings")
	}
}

func TestChatCLIConcurrentSettingsKeepNewDeliveryPending(t *testing.T) {
	f := setup(t)
	id, _ := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	f.control.provisionCancel()
	<-f.control.provisionDone
	if _, err := f.db.SQL.Exec("UPDATE chat_cli_settings SET server='https://control.test'"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	node, server := worker(t, strings.Repeat("e", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		close(entered)
		<-release
		return 200, map[string]any{"ok": true, "installed": 1}, nil
	}))
	add(t, f, node, server, "Concurrent CLI")
	done := make(chan error, 1)
	go func() { done <- f.control.syncMemberCLI(context.Background(), id, "alice") }()
	<-entered
	if err := f.control.queueChatCLI(id, node.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.db.SQL.QueryRow("SELECT state FROM member_cli_sync WHERE member_id=? AND node_id=?", id, node.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatal("new delivery was overwritten", state, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.control.syncMemberCLI(ctx, id, "alice"); err != context.Canceled {
		t.Fatal(err)
	}
	if err := f.db.SQL.QueryRow("SELECT state FROM member_cli_sync WHERE member_id=? AND node_id=?", id, node.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatal("cancelled delivery lost", state, err)
	}
}
