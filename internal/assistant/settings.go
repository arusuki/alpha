package assistant

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

var builtinTools = []string{"list_nodes", "get_node_inventory", "get_node_storage", "get_node_containers", "get_node_processes", "get_node_gpu"}
var serverID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,15}$`)
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

const settingsKeyFile = "assistant-mcp.key"
const settingsKeyAAD = "project-alpha/assistant-mcp/v1"

type MCPServer struct {
	ID      string   `json:"id"`
	URL     string   `json:"url"`
	Enabled bool     `json:"enabled"`
	Tools   []string `json:"tools"`
}
type Settings struct {
	Instructions string      `json:"instructions"`
	BuiltinTools []string    `json:"builtin_tools"`
	MCPServers   []MCPServer `json:"mcp_servers"`
}
type settingsView struct {
	Revision       int64           `json:"revision"`
	Value          Settings        `json:"value"`
	HasTokens      map[string]bool `json:"has_tokens"`
	AvailableTools []string        `json:"available_tools"`
}

func (c Settings) validate() error {
	bad := func(text string) error { return httpapi.NewError(400, text) }
	if strings.TrimSpace(c.Instructions) == "" || utf8.RuneCountInString(c.Instructions) > 16000 {
		return bad("提示词需为 1–16000 个字符")
	}
	if c.BuiltinTools == nil || c.MCPServers == nil || len(c.MCPServers) > 8 {
		return bad("工具列表不能为空值，MCP 服务最多 8 个")
	}
	seen := map[string]bool{}
	for _, name := range c.BuiltinTools {
		if !slices.Contains(builtinTools, name) || seen[name] {
			return bad("内置工具无效或重复")
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	exposed := map[string]bool{}
	for _, s := range c.MCPServers {
		if !serverID.MatchString(s.ID) || seen[s.ID] {
			return bad("MCP 标识需为小写字母开头的 1–16 位字母、数字或下划线，且不能重复")
		}
		seen[s.ID] = true
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(s.URL) > 2048 {
			return bad("MCP 地址需为 HTTP(S)，不能包含账号、查询参数或片段")
		}
		if s.Tools == nil || len(s.Tools) > 32 || (s.Enabled && len(s.Tools) == 0) {
			return bad("启用 MCP 服务时须填写允许的工具名称，最多 32 个")
		}
		names := map[string]bool{}
		for _, name := range s.Tools {
			if !toolName.MatchString(name) || names[name] {
				return bad("MCP 工具名需为 1–40 位字母、数字、下划线或连字符，且不能重复")
			}
			names[name] = true
			if s.Enabled {
				publicName := "mcp_" + s.ID + "_" + name
				if exposed[publicName] {
					return bad("MCP 服务与工具组合产生重复名称，请调整服务标识")
				}
				exposed[publicName] = true
			}
		}
	}
	return nil
}
func decodeSettings(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
func (h *Handler) settings() (Settings, map[string]string, int64, error) {
	return h.loadSettings(true)
}

func (h *Handler) loadSettings(secrets bool) (Settings, map[string]string, int64, error) {
	var c Settings
	var raw, cipher string
	var revision int64
	tokens := map[string]string{}
	err := h.db.SQL.QueryRow("SELECT value,secrets_ciphertext,revision FROM assistant_settings WHERE id=1").Scan(&raw, &cipher, &revision)
	if err != nil {
		return c, nil, 0, err
	}
	if err = decodeSettings([]byte(raw), &c); err == nil {
		err = c.validate()
	}
	if err != nil {
		return c, nil, 0, fmt.Errorf("invalid assistant settings; use a new data directory: %w", err)
	}
	if secrets && cipher != "" {
		plain, e := credentials.Decrypt(h.db.Directory, settingsKeyFile, settingsKeyAAD, cipher)
		if e != nil {
			return c, nil, 0, e
		}
		if e = json.Unmarshal([]byte(plain), &tokens); e != nil || tokens == nil {
			return c, nil, 0, fmt.Errorf("invalid MCP credential data")
		}
	}
	return c, tokens, revision, nil
}
func publicSettings(c Settings, tokens map[string]string, revision int64) settingsView {
	has := map[string]bool{}
	for _, s := range c.MCPServers {
		has[s.ID] = tokens[s.ID] != ""
	}
	return settingsView{revision, c, has, builtinTools}
}
func (h *Handler) dispatchSettings(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	c, tokens, revision, err := h.settings()
	if err != nil {
		return 0, nil, err
	}
	if r.Method == "GET" {
		return 200, publicSettings(c, tokens, revision), nil
	}
	if r.Method != "PUT" {
		return 0, nil, httpapi.NewError(405, "不支持的配置操作")
	}
	var body struct {
		Revision    int64             `json:"revision"`
		Value       Settings          `json:"value"`
		Tokens      map[string]string `json:"tokens"`
		ClearTokens []string          `json:"clear_tokens"`
	}
	if err = httpapi.DecodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	if body.Revision != revision {
		return 0, nil, httpapi.NewError(409, "助理配置已被修改，请重新载入")
	}
	body.Value.Instructions = strings.TrimSpace(body.Value.Instructions)
	if err = body.Value.validate(); err != nil {
		return 0, nil, err
	}
	old := map[string]MCPServer{}
	for _, s := range c.MCPServers {
		old[s.ID] = s
	}
	next := map[string]string{}
	ids := map[string]bool{}
	for _, s := range body.Value.MCPServers {
		ids[s.ID] = true
		// Changing the endpoint must never forward an existing credential to it.
		if prior, ok := old[s.ID]; ok && prior.URL == s.URL {
			next[s.ID] = tokens[s.ID]
		}
		if token, ok := body.Tokens[s.ID]; ok && token != "" {
			next[s.ID] = token
		}
		if slices.Contains(body.ClearTokens, s.ID) {
			next[s.ID] = ""
		}
	}
	for id, token := range body.Tokens {
		if !ids[id] || len(token) > 8192 || strings.ContainsAny(token, "\r\n") {
			return 0, nil, httpapi.NewError(400, "MCP 认证标识或 Token 无效")
		}
	}
	for _, id := range body.ClearTokens {
		if !ids[id] {
			return 0, nil, httpapi.NewError(400, "清除 Token 的服务不存在")
		}
	}
	cipher := ""
	has := false
	for _, v := range next {
		has = has || v != ""
	}
	if has {
		raw, _ := json.Marshal(next)
		cipher, err = credentials.Encrypt(h.db.Directory, settingsKeyFile, settingsKeyAAD, string(raw))
		if err != nil {
			return 0, nil, err
		}
	}
	raw, _ := json.Marshal(body.Value)
	err = h.db.Transaction(func(tx *sql.Tx) error {
		result, e := tx.Exec("UPDATE assistant_settings SET value=?,secrets_ciphertext=?,revision=revision+1 WHERE id=1 AND revision=?", string(raw), cipher, revision)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return httpapi.NewError(409, "助理配置已被修改，请重新载入")
		}
		return platform.Audit(tx, user.Username, "assistant.settings", "更新提示词与工具配置")
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, publicSettings(body.Value, next, revision+1), nil
}
