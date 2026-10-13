package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"project-alpha/internal/agent"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func setup(t *testing.T) *Handler {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), func(tx *sql.Tx) error {
		if err := platform.InstallAssistant(tx); err != nil {
			return err
		}
		if err := agent.Initialize(tx); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO service_identity VALUES(1,'control','control');
INSERT INTO users VALUES('alice','alice','unused','admin',1,0),('bob','bob','unused','admin',1,0),('viewer','viewer','unused','viewer',1,0);
UPDATE agent_settings SET value=json_set(value,'$.model','test');`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(db, func(context.Context, platform.User, ToolCall) (any, error) { return nil, nil })
	t.Cleanup(func() { h.Close(); db.SQL.Close() })
	return h
}
func call(h *Handler, user, method, path, body string) (int, any, error) {
	role := "admin"
	if user == "viewer" {
		role = "viewer"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return h.Dispatch(httptest.NewRecorder(), req, platform.User{ID: user, Username: user, Role: role})
}
func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var api *httpapi.Error
	if !errors.As(err, &api) || api.Status != want {
		t.Fatalf("want %d, got %v", want, err)
	}
}
func output(in input) result {
	state, _ := json.Marshal(map[string]any{"user_id": in.UserID, "session_id": in.SessionID})
	memory, _ := json.Marshal([]map[string]string{{"user_id": in.UserID, "memory": "GPU first"}})
	return result{"ready", state, memory}
}
func TestSessionMemoryIsolationAndFailureAtomicity(t *testing.T) {
	h := setup(t)
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		return output(in), nil
	}
	_, v, err := call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := v.(map[string]any)["id"].(string)
	_, _, err = call(h, "bob", "GET", "/api/assistant/sessions/"+id, "")
	assertStatus(t, err, 404)
	_, _, err = call(h, "bob", "POST", "/api/assistant/sessions/"+id+"/messages", `{"message":"read"}`)
	assertStatus(t, err, 404)
	_, _, err = call(h, "viewer", "GET", "/api/assistant/memories", "")
	assertStatus(t, err, 403)
	_, v, err = call(h, "bob", "GET", "/api/assistant/memories", "")
	if err != nil || string(v.(map[string]any)["memories"].(json.RawMessage)) != "[]" {
		t.Fatal(v, err)
	}
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		if !strings.Contains(string(in.Memories), "GPU first") || in.Session != nil {
			t.Fatal("cross-session memory missing", in)
		}
		return output(in), nil
	}
	if _, _, err = call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"another conversation"}`); err != nil {
		t.Fatal(err)
	}
	before, err := platform.Rows(h.db.SQL, "SELECT * FROM assistant_sessions ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(before)
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		return output(in), errors.New("provider failure")
	}
	if _, _, err = call(h, "alice", "POST", "/api/assistant/sessions/"+id+"/messages", `{"message":"failed"}`); err == nil {
		t.Fatal("failure ignored")
	}
	after, _ := platform.Rows(h.db.SQL, "SELECT * FROM assistant_sessions ORDER BY id")
	raw, _ := json.Marshal(after)
	if string(old) != string(raw) {
		t.Fatal("failed run modified stored history")
	}
	if _, err = h.db.SQL.Exec("CREATE TRIGGER reject_memory BEFORE UPDATE ON assistant_memories BEGIN SELECT RAISE(ABORT,'failure'); END"); err != nil {
		t.Fatal(err)
	}
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		return output(in), nil
	}
	if _, _, err = call(h, "alice", "POST", "/api/assistant/sessions/"+id+"/messages", `{"message":"rollback"}`); err == nil {
		t.Fatal("storage failure ignored")
	}
	after, _ = platform.Rows(h.db.SQL, "SELECT * FROM assistant_sessions ORDER BY id")
	raw, _ = json.Marshal(after)
	if string(old) != string(raw) {
		t.Fatal("memory failure did not roll back session")
	}
	if _, _, err = call(h, "alice", "DELETE", "/api/assistant/memories", ""); err != nil {
		t.Fatal(err)
	}
	_, v, err = call(h, "alice", "GET", "/api/assistant/memories", "")
	if err != nil || string(v.(map[string]any)["memories"].(json.RawMessage)) != "[]" {
		t.Fatal(v, err)
	}
	if _, _, err = call(h, "alice", "GET", "/api/assistant/sessions/"+id, ""); err != nil {
		t.Fatal("clearing memory deleted session", err)
	}
}
func TestBusyRevocationAndInvalidRuntimeState(t *testing.T) {
	h := setup(t)
	release, err := h.acquire("alice")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"hello"}`)
	assertStatus(t, err, 409)
	_, _, err = call(h, "alice", "DELETE", "/api/assistant/memories", "")
	assertStatus(t, err, 409)
	release()
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		in.UserID = "bob"
		return output(in), nil
	}
	_, _, err = call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"hello"}`)
	assertStatus(t, err, 502)
	h.run = func(_ context.Context, in input, tool func(ToolCall) (any, error)) (result, error) {
		if _, err := h.db.SQL.Exec("UPDATE users SET enabled=0 WHERE id='alice'"); err != nil {
			t.Fatal(err)
		}
		_, err := tool(ToolCall{Name: "list_nodes"})
		assertStatus(t, err, 403)
		return output(in), nil
	}
	_, _, err = call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"hello"}`)
	assertStatus(t, err, 403)
	var n int
	if err = h.db.SQL.QueryRow("SELECT count(*) FROM assistant_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
func TestRuntimeUnavailable(t *testing.T) {
	client := &serviceClient{socket: filepath.Join(t.TempDir(), "absent.sock")}
	_, err := client.run(context.Background(), input{}, nil)
	assertStatus(t, err, 503)
	if health := client.health(context.Background()); health.Status != "unavailable" {
		t.Fatal(health)
	}
}

func startAgnoService(t *testing.T) *serviceClient {
	t.Helper()
	python := os.Getenv("ALPHA_AGNO_TEST_PYTHON")
	if python == "" {
		t.Skip("set ALPHA_AGNO_TEST_PYTHON to test the real container service code")
	}
	path := filepath.Join(t.TempDir(), "agno.sock")
	source, err := filepath.Abs("../../runtime/agno/service.py")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-I", "-u", source, "--socket", path)
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	client := &serviceClient{socket: path}
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if health := client.health(context.Background()); health.Status == "ready" {
			return client
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Agno service did not become ready")
	return nil
}

// Enabled by CI and local validation when the pinned Python environment exists.
func TestRealAgnoService(t *testing.T) {
	client := startAgnoService(t)
	h := setup(t)
	h.runtime = client
	h.run = client.run
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong model endpoint %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("unconfigured key inherited")
		}
		message := map[string]any{"role": "assistant", "content": "没有注册节点。"}
		reason := "stop"
		if n == 1 {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "list-1", "type": "function", "function": map[string]any{"name": "list_nodes", "arguments": "{}"}}}
			reason = "tool_calls"
		}
		httpapi.WriteJSON(w, 200, map[string]any{"id": "completion", "object": "chat.completion", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}})
	}))
	defer server.Close()
	if _, err := h.db.SQL.Exec("UPDATE agent_settings SET value=json_set(value,'$.protocol','completions','$.endpoint',?)", server.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	var calls int
	h.tools = func(_ context.Context, user platform.User, call ToolCall) (any, error) {
		calls++
		if user.ID != "alice" || call.Name != "list_nodes" {
			t.Fatal(user, call)
		}
		return map[string]any{"nodes": []any{}}, nil
	}
	_, value, err := call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"列出节点"}`)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || requests.Load() != 2 {
		t.Fatal("tool loop not executed", calls, requests.Load())
	}
	id := value.(map[string]any)["id"].(string)
	// The service starts a new isolated run using the previous persisted state.
	if _, _, err = call(h, "alice", "POST", "/api/assistant/sessions/"+id+"/messages", `{"message":"继续"}`); err != nil {
		t.Fatal(err)
	}
	_, messages, _, err := h.session("assistant_sessions", id, "alice")
	if err != nil || len(messages) != 4 {
		t.Fatal(messages, err)
	}
}

func TestRealAgnoCancellation(t *testing.T) {
	client := startAgnoService(t)
	h := setup(t)
	h.runtime = client
	h.run = client.run
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	if _, err := h.db.SQL.Exec("UPDATE agent_settings SET value=json_set(value,'$.protocol','completions','$.endpoint',?)", server.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/api/assistant/sessions", strings.NewReader(`{"message":"wait"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		_, _, err := h.Dispatch(httptest.NewRecorder(), req, platform.User{ID: "alice", Username: "alice", Role: "admin"})
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal("run exited before model call", err)
	case <-time.After(15 * time.Second):
		t.Fatal("model request never started")
	}
	if health := h.Health(context.Background()); health.Status != "ready" || health.ActiveRuns != 1 {
		t.Fatal("health unavailable during model call", health)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled run succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Agno child did not stop")
	}
	until := time.Now().Add(5 * time.Second)
	for h.Health(context.Background()).ActiveRuns != 0 && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if health := h.Health(context.Background()); health.Status != "ready" || health.ActiveRuns != 0 {
		t.Fatal("cancelled child still running", health)
	}
	var n int
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM assistant_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatal("cancelled run saved", n, err)
	}
}
