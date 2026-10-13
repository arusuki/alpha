package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func memberLogin(t *testing.T, h *Handler, id, name string) string {
	t.Helper()
	hash, err := platform.PasswordHash("Member-password-123", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.db.SQL.Exec(`INSERT INTO members VALUES(?,?, '{}','{}','[]',?,'unused','existing-public-key',?,'active','invite','hash',1)`, id, name, hash, id)
	if err != nil {
		t.Fatal(err)
	}
	token, err := (&members.Store{Database: h.db}).LoginStatus(name, "Member-password-123")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func memberCall(h *Handler, token, method, path, body string) (int, any, error) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return h.DispatchMember(httptest.NewRecorder(), req, token)
}

func TestMemberIdentityIsolationAndRevocation(t *testing.T) {
	h := setup(t)
	if err := h.db.Transaction(members.Initialize); err != nil {
		t.Fatal(err)
	}
	// Deliberately collide with administrator IDs to test both persistence and runtime identity.
	alice := memberLogin(t, h, "alice", "alice")
	bob := memberLogin(t, h, "bob", "bob")
	cfg, _, rev, err := h.settings()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCPServers = []MCPServer{{ID: "admin", URL: "http://127.0.0.1/mcp", Enabled: true, Tools: []string{"delete"}}}
	body, _ := json.Marshal(map[string]any{"revision": rev, "value": cfg, "tokens": map[string]string{"admin": "admin-secret"}})
	if _, _, err := call(h, "alice", "PUT", "/api/assistant/settings", string(body)); err != nil {
		t.Fatal(err)
	}
	toolCalls := 0
	h.tools = func(_ context.Context, u platform.User, c ToolCall) (any, error) {
		toolCalls++
		if u.Role != "member" || u.ID != "alice" || u.Username != "member:alice" {
			t.Fatal("untrusted identity", u)
		}
		return map[string]bool{"ok": true}, nil
	}
	h.run = func(_ context.Context, in input, tool func(ToolCall) (any, error)) (result, error) {
		if in.UserID != "member:alice" || len(in.Settings.MCPServers) != 0 || len(in.MCPTokens) != 0 {
			t.Fatal("member received admin identity or MCP credentials")
		}
		raw, _ := json.Marshal(in)
		if strings.Contains(string(raw), alice) {
			t.Fatal("member token entered runtime")
		}
		if _, err := tool(ToolCall{Name: "list_nodes"}); err != nil {
			t.Fatal(err)
		}
		_, err := tool(ToolCall{Name: "mcp:admin:delete"})
		assertStatus(t, err, 403)
		return output(in), nil
	}
	_, v, err := memberCall(h, alice, "POST", "/api/assistant/sessions", `{"message":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if toolCalls != 1 {
		t.Fatal(toolCalls)
	}
	id := v.(map[string]any)["id"].(string)
	for _, method := range []string{"GET", "POST"} {
		path := "/api/assistant/sessions/" + id
		if method == "POST" {
			path += "/messages"
		}
		_, _, err := memberCall(h, bob, method, path, `{"message":"steal"}`)
		assertStatus(t, err, 404)
	}
	_, _, err = call(h, "alice", "GET", "/api/assistant/sessions/"+id, "")
	assertStatus(t, err, 404)
	_, mem, err := memberCall(h, bob, "GET", "/api/assistant/memories", "")
	if err != nil || string(mem.(map[string]any)["memories"].(json.RawMessage)) != "[]" {
		t.Fatal("memory crossed users", err)
	}
	for _, path := range []string{"/api/assistant/settings", "/api/assistant/health"} {
		_, _, err := memberCall(h, alice, "GET", path, "")
		assertStatus(t, err, 403)
	}
	h.run = func(_ context.Context, in input, tool func(ToolCall) (any, error)) (result, error) {
		if !strings.Contains(string(in.Memories), "GPU first") {
			t.Fatal("member memory lost")
		}
		if err := (&members.Store{Database: h.db}).Logout(alice); err != nil {
			t.Fatal(err)
		}
		_, err := tool(ToolCall{Name: "list_nodes"})
		assertStatus(t, err, 401)
		return output(in), nil
	}
	_, _, err = memberCall(h, alice, "POST", "/api/assistant/sessions/"+id+"/messages", `{"message":"revoked"}`)
	assertStatus(t, err, 401)
	if toolCalls != 1 {
		t.Fatal("revoked token reached tool")
	}
	var n int
	if err := h.db.SQL.QueryRow("SELECT json_array_length(messages) FROM member_assistant_sessions WHERE id=?", id).Scan(&n); err != nil || n != 2 {
		t.Fatal("revoked run committed", err, n)
	}
	_, _, err = memberCall(h, alice, "GET", "/api/assistant/sessions", "")
	assertStatus(t, err, 401)
	if _, err := h.db.SQL.Exec("UPDATE member_sessions SET expires_at=0 WHERE member_id='bob'"); err != nil {
		t.Fatal(err)
	}
	_, _, err = memberCall(h, bob, "GET", "/api/assistant/sessions", "")
	assertStatus(t, err, 401)
	if _, err := h.db.SQL.Exec("DELETE FROM members WHERE id='alice'"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM member_assistant_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatal("member deletion left conversations", err)
	}
}

func TestMemberCommitRevalidatesTokenAndRollsBack(t *testing.T) {
	h := setup(t)
	if err := h.db.Transaction(members.Initialize); err != nil {
		t.Fatal(err)
	}
	token := memberLogin(t, h, "member", "alice")
	h.run = func(_ context.Context, in input, _ func(ToolCall) (any, error)) (result, error) {
		return output(in), nil
	}
	if _, err := h.db.SQL.Exec(`CREATE TRIGGER fail_memory BEFORE INSERT ON member_assistant_memories BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, _, err := memberCall(h, token, "POST", "/api/assistant/sessions", `{"message":"rollback"}`)
	if err == nil {
		t.Fatal("missing rollback")
	}
	var n int
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM member_assistant_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	// The authorizer is also usable inside SQLite's write transaction, avoiding a
	// revoked credential being committed between the final check and transaction.
	if err := h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM member_sessions"); err != nil {
			return err
		}
		_, err := members.SessionUser(tx, token)
		assertStatus(t, err, 401)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
