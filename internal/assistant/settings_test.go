package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsSecretsRevisionAndRuntime(t *testing.T) {
	h := setup(t)
	_, public, err := call(h, "alice", "GET", "/api/assistant/settings", "")
	if err != nil {
		t.Fatal(err)
	}
	view := public.(settingsView)
	view.Value.Instructions = "先检查 GPU，引用观测时间"
	view.Value.BuiltinTools = []string{"list_nodes"}
	view.Value.MCPServers = []MCPServer{{ID: "metrics", URL: "http://127.0.0.1:9010/mcp", Enabled: true, Tools: []string{"status"}}}
	save := func(revision int64, tokens map[string]string, clear []string) (any, error) {
		raw, _ := json.Marshal(map[string]any{"revision": revision, "value": view.Value, "tokens": tokens, "clear_tokens": clear})
		_, out, err := call(h, "alice", "PUT", "/api/assistant/settings", string(raw))
		return out, err
	}
	out, err := save(view.Revision, map[string]string{"metrics": "private-mcp-token"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "private-mcp-token") || !out.(settingsView).HasTokens["metrics"] {
		t.Fatal("secret exposed or lost", string(raw))
	}
	var stored, cipher string
	if err = h.db.SQL.QueryRow("SELECT value,secrets_ciphertext FROM assistant_settings").Scan(&stored, &cipher); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored+cipher, "private-mcp-token") || !strings.HasPrefix(cipher, "v1:") {
		t.Fatal("plaintext token persisted")
	}
	_, err = save(view.Revision, nil, nil)
	assertStatus(t, err, 409)
	out, err = save(2, nil, nil)
	if err != nil || !out.(settingsView).HasTokens["metrics"] {
		t.Fatal("empty token update failed", err)
	}
	h.run = func(ctx context.Context, in input, tool func(ToolCall) (any, error)) (result, error) {
		if in.Settings.Instructions != view.Value.Instructions || in.MCPTokens["metrics"] != "private-mcp-token" {
			t.Fatal("runtime did not receive settings")
		}
		if _, err := tool(ToolCall{Name: "get_node_gpu"}); err == nil {
			t.Fatal("disabled builtin allowed")
		}
		if _, err := tool(ToolCall{Name: "mcp:metrics:delete"}); err == nil {
			t.Fatal("unlisted MCP tool allowed")
		}
		if _, err := tool(ToolCall{Name: "mcp:metrics:status"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.SQL.Exec("UPDATE users SET enabled=0 WHERE id='alice'"); err != nil {
			t.Fatal(err)
		}
		if _, err := tool(ToolCall{Name: "mcp:metrics:status"}); err == nil {
			t.Fatal("revoked admin allowed MCP")
		}
		return output(in), nil
	}
	_, _, err = call(h, "alice", "POST", "/api/assistant/sessions", `{"message":"检查"}`)
	assertStatus(t, err, 403)
	if _, err = h.db.SQL.Exec("UPDATE users SET enabled=1 WHERE id='alice'"); err != nil {
		t.Fatal(err)
	}
	view.Value.MCPServers[0].URL = "http://127.0.0.1:9011/mcp"
	out, err = save(3, nil, nil)
	if err != nil || out.(settingsView).HasTokens["metrics"] {
		t.Fatal("token forwarded to changed endpoint", err)
	}
	out, err = save(4, map[string]string{"metrics": "replacement"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err = save(5, nil, []string{"metrics"})
	if err != nil || out.(settingsView).HasTokens["metrics"] {
		t.Fatal("clear token failed", err)
	}
	_, _, err = call(h, "viewer", "GET", "/api/assistant/settings", "")
	assertStatus(t, err, 403)
}
func TestSettingsValidation(t *testing.T) {
	h := setup(t)
	cfg, _, _, err := h.settings()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Settings){
		func(c *Settings) { c.Instructions = "" },
		func(c *Settings) { c.BuiltinTools = []string{"shell"} },
		func(c *Settings) {
			c.MCPServers = []MCPServer{{ID: "x", URL: "http://user:pass@host/mcp", Enabled: true, Tools: []string{"status"}}}
		},
		func(c *Settings) {
			c.MCPServers = []MCPServer{{ID: "x", URL: "http://host/mcp", Enabled: true, Tools: []string{}}}
		},
		func(c *Settings) {
			c.MCPServers = []MCPServer{{ID: "x", URL: "http://host/mcp", Enabled: true, Tools: []string{"status", "status"}}}
		},
	} {
		bad := cfg
		mutate(&bad)
		if bad.validate() == nil {
			t.Fatal("invalid config accepted", bad)
		}
	}
}
