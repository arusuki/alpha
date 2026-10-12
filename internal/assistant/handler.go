// Package assistant hosts the control-only Agno operations assistant.
package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"project-alpha/internal/agent"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type ToolCall struct {
	Name   string `json:"name"`
	NodeID string `json:"node_id"`
}
type ToolFunc func(context.Context, platform.User, ToolCall) (any, error)
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type input struct {
	Settings  Settings          `json:"settings"`
	MCPTokens map[string]string `json:"mcp_tokens"`
	UserID    string            `json:"user_id"`
	SessionID string            `json:"session_id"`
	Message   string            `json:"message"`
	Config    agent.Config      `json:"config"`
	APIKey    string            `json:"api_key"`
	Session   json.RawMessage   `json:"session"`
	Memories  json.RawMessage   `json:"memories"`
}
type result struct {
	Content  string          `json:"content"`
	Session  json.RawMessage `json:"session"`
	Memories json.RawMessage `json:"memories"`
}
type runner func(context.Context, input, func(ToolCall) (any, error)) (result, error)

type Handler struct {
	db      *platform.Database
	tools   ToolFunc
	run     runner
	runtime *serviceClient
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	busy    map[string]bool
	slots   chan struct{}
}

func New(db *platform.Database, tools ToolFunc) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := newServiceClient(filepath.Join(db.Directory, "agno", "agno.sock"))
	return &Handler{db: db, tools: tools, runtime: runtime, run: runtime.run, ctx: ctx, cancel: cancel, busy: map[string]bool{}, slots: make(chan struct{}, 4)}
}
func (h *Handler) Close() { h.cancel() }

var sessionRoute = regexp.MustCompile(`^/api/assistant/sessions/([a-f0-9]{32})(/messages)?$`)

func (h *Handler) authorize(user platform.User) error {
	var count int
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM users WHERE id=? AND role='admin' AND enabled=1", user.ID).Scan(&count); err != nil {
		return err
	}
	if user.Role != "admin" || count != 1 {
		return httpapi.NewError(403, "运维助理需要有效的总控管理员权限")
	}
	return nil
}
func (h *Handler) acquire(userID string) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return nil, httpapi.NewError(503, "总控正在关闭")
	}
	if h.busy[userID] {
		return nil, httpapi.NewError(409, "你已有运维助理请求正在执行，请等待完成或停止")
	}
	select {
	case h.slots <- struct{}{}:
	default:
		return nil, httpapi.NewError(429, "运维助理繁忙，请稍后重试")
	}
	h.busy[userID] = true
	return func() { h.mu.Lock(); delete(h.busy, userID); h.mu.Unlock(); <-h.slots }, nil
}
func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if err := h.authorize(user); err != nil {
		return 0, nil, err
	}
	path := r.URL.Path
	if path == "/api/assistant/settings" {
		return h.dispatchSettings(w, r, user)
	}
	if path == "/api/assistant/health" && r.Method == "GET" {
		return 200, h.Health(r.Context()), nil
	}
	if path == "/api/assistant/sessions" && r.Method == "GET" {
		rows, err := platform.Rows(h.db.SQL, "SELECT id,title,created_at,updated_at FROM assistant_sessions WHERE user_id=? ORDER BY updated_at DESC LIMIT 100", user.ID)
		return 200, map[string]any{"sessions": rows}, err
	}
	if path == "/api/assistant/memories" {
		if r.Method == "GET" {
			raw, err := h.memories(user.ID)
			return 200, map[string]any{"memories": raw}, err
		}
		if r.Method == "DELETE" {
			release, err := h.acquire(user.ID)
			if err != nil {
				return 0, nil, err
			}
			defer release()
			err = h.db.Transaction(func(tx *sql.Tx) error {
				if _, err := tx.Exec("DELETE FROM assistant_memories WHERE user_id=?", user.ID); err != nil {
					return err
				}
				return platform.Audit(tx, user.Username, "assistant.memories.clear", "清空本人的跨会话记忆")
			})
			return 200, map[string]bool{"ok": true}, err
		}
	}
	id := ""
	if match := sessionRoute.FindStringSubmatch(path); match != nil {
		id = match[1]
		if r.Method == "GET" && match[2] == "" {
			_, messages, title, err := h.session(id, user.ID)
			return 200, map[string]any{"id": id, "title": title, "messages": messages}, err
		}
		if r.Method != "POST" || match[2] != "/messages" {
			return 0, nil, httpapi.NewError(404, "运维助理接口不存在")
		}
	} else if path != "/api/assistant/sessions" || r.Method != "POST" {
		return 0, nil, httpapi.NewError(404, "运维助理接口不存在")
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := httpapi.DecodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" || utf8.RuneCountInString(body.Message) > 8000 {
		return 0, nil, httpapi.NewError(400, "消息需为 1–8000 个字符")
	}
	release, err := h.acquire(user.ID)
	if err != nil {
		return 0, nil, err
	}
	defer release()
	var saved json.RawMessage
	messages := []Message{}
	title := string([]rune(body.Message)[:min(80, utf8.RuneCountInString(body.Message))])
	if id != "" {
		saved, messages, title, err = h.session(id, user.ID)
		if err != nil {
			return 0, nil, err
		}
	} else {
		id = platform.RandomHex(16)
	}
	memories, err := h.memories(user.ID)
	if err != nil {
		return 0, nil, err
	}
	cfg, err := agent.NewStore(h.db).RuntimeConfig()
	if err != nil {
		return 0, nil, err
	}
	settings, tokens, _, err := h.settings()
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	// This single request can include multiple model/tool turns.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(16 * time.Minute))
	calls := []ToolCall{}
	out, err := h.run(ctx, input{UserID: user.ID, SessionID: id, Message: body.Message, Config: cfg, APIKey: cfg.APIKey, Session: saved, Memories: memories, Settings: settings, MCPTokens: tokens}, func(call ToolCall) (any, error) {
		if err := h.authorize(user); err != nil {
			return nil, err
		}
		if strings.HasPrefix(call.Name, "mcp:") {
			parts := strings.Split(call.Name, ":")
			if len(parts) == 3 && call.NodeID == "" {
				for _, server := range settings.MCPServers {
					if server.Enabled && server.ID == parts[1] && slices.Contains(server.Tools, parts[2]) {
						calls = append(calls, call)
						return map[string]bool{"allowed": true}, nil
					}
				}
			}
			return nil, httpapi.NewError(403, "MCP 工具未授权")
		}
		if !slices.Contains(settings.BuiltinTools, call.Name) {
			return nil, httpapi.NewError(403, "此内置工具已禁用")
		}
		calls = append(calls, call)
		return h.tools(ctx, user, call)
	})
	if err != nil {
		return 0, nil, err
	}
	if err = ctx.Err(); err != nil {
		return 0, nil, httpapi.NewError(408, "运维助理请求已取消或超时，本轮未保存")
	}
	if err = h.authorize(user); err != nil {
		return 0, nil, err
	}
	// Treat even the local runtime as a protocol boundary; never accept a result
	// belonging to another user/session or an invalid state envelope.
	var state struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}
	var memoryRows []struct {
		UserID string `json:"user_id"`
	}
	if strings.TrimSpace(out.Content) == "" || json.Unmarshal(out.Session, &state) != nil || state.UserID != user.ID || state.SessionID != id || json.Unmarshal(out.Memories, &memoryRows) != nil || string(out.Memories) == "null" {
		return 0, nil, httpapi.NewError(502, "Agno 返回了无效的会话或记忆，本轮未保存")
	}
	for _, m := range memoryRows {
		if m.UserID != user.ID {
			return 0, nil, httpapi.NewError(502, "Agno 记忆归属无效，本轮未保存")
		}
	}
	messages = append(messages, Message{"user", body.Message}, Message{"assistant", out.Content})
	raw, _ := json.Marshal(messages)
	now := platform.Now()
	err = h.db.Transaction(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO assistant_sessions(id,user_id,title,data,messages,created_at,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET data=excluded.data,messages=excluded.messages,updated_at=excluded.updated_at WHERE assistant_sessions.user_id=excluded.user_id`, id, user.ID, title, string(out.Session), string(raw), now, now)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO assistant_memories(user_id,data,updated_at) VALUES(?,?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, user.ID, string(out.Memories), now); err != nil {
			return err
		}
		return platform.Audit(tx, user.Username, "assistant.run", id)
	})
	return 200, map[string]any{"id": id, "title": title, "messages": messages, "tools": calls}, err
}
func (h *Handler) session(id, userID string) (json.RawMessage, []Message, string, error) {
	var data, raw, title string
	err := h.db.SQL.QueryRow("SELECT data,messages,title FROM assistant_sessions WHERE id=? AND user_id=?", id, userID).Scan(&data, &raw, &title)
	if err == sql.ErrNoRows {
		err = httpapi.NewError(404, "运维助理会话不存在")
	}
	if err != nil {
		return nil, nil, "", err
	}
	var messages []Message
	if err = json.Unmarshal([]byte(raw), &messages); err != nil {
		return nil, nil, "", err
	}
	return json.RawMessage(data), messages, title, nil
}
func (h *Handler) memories(userID string) (json.RawMessage, error) {
	var raw string
	err := h.db.SQL.QueryRow("SELECT data FROM assistant_memories WHERE user_id=?", userID).Scan(&raw)
	if err == sql.ErrNoRows {
		return json.RawMessage("[]"), nil
	}
	return json.RawMessage(raw), err
}
