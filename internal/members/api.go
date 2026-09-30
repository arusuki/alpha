package members

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type attempt struct {
	count int
	since time.Time
}
type Handler struct {
	Reserve    func(*sql.Tx, Member) error
	Registered func(Member)
	store      *Store
	mu         sync.Mutex
	attempts   map[string]attempt
}

func NewHandler(db *platform.Database) *Handler {
	return &Handler{store: &Store{db}, attempts: map[string]attempt{}}
}

func (h *Handler) RegisterRegistry(req Registration, token string) (Member, error) {
	if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return Member{}, httpapi.NewError(400, "注册资源令牌格式无效")
	}
	m, err := h.store.registerWithToken(req, h.Reserve, token)
	if err == nil && h.Registered != nil {
		h.Registered(m)
	}
	return m, err
}
func IsRoute(path string) bool {
	return path == "/api/members" || strings.HasPrefix(path, "/api/members/") || path == invitationPage || strings.HasPrefix(path, invitationPage+"/")
}
func decode(w http.ResponseWriter, r *http.Request, target any) error {
	value, err := httpapi.RequestBody(w, r)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return httpapi.NewError(400, "请求字段无效："+err.Error())
	}
	return nil
}
func (h *Handler) checkAttempts(r *http.Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for ip, a := range h.attempts {
		if now.Sub(a.since) >= 5*time.Minute {
			delete(h.attempts, ip)
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	a, exists := h.attempts[ip]
	if a.count >= 20 || !exists && len(h.attempts) >= 1024 {
		return httpapi.NewError(429, "注册尝试过多，请在 5 分钟后重试")
	}
	if !exists {
		a.since = now
	}
	a.count++
	h.attempts[ip] = a
	return nil
}

// DispatchPublic exposes only the schema and invitation-backed registration.
// It runs after the platform Host/Origin guards, without creating a session.
func (h *Handler) DispatchPublic(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if r.Method == "GET" && r.URL.Path == "/api/members/registration-schema" {
		v, err := h.store.Schema()
		return 200, v, err
	}
	if r.Method == "POST" && r.URL.Path == "/api/members/register" {
		if err := h.checkAttempts(r); err != nil {
			return 0, nil, err
		}
		var req Registration
		if err := decode(w, r, &req); err != nil {
			return 0, nil, err
		}
		v, err := h.store.RegisterWith(req, h.Reserve)
		if err != nil {
			return 0, nil, err
		}
		if h.Registered != nil {
			h.Registered(v)
		}
		return 201, map[string]any{"resource_token": v.ResourceToken, "resource_status": "pending", "id": v.ID, "username": v.Username, "profile": v.Profile, "schema_revision": v.Schema.Revision, "created_at": v.CreatedAt}, nil
	}
	return 0, nil, nil
}

func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	path := r.URL.Path
	if path == invitationPage || strings.HasPrefix(path, invitationPage+"/") {
		return h.invitationView(w, r, user.Username)
	}
	if path == "/api/members/registration-schema" && r.Method == "PUT" {
		var req Schema
		if err := decode(w, r, &req); err != nil {
			return 0, nil, err
		}
		v, err := h.store.SaveSchema(req, user.Username)
		return 200, v, err
	}
	if path == "/api/members" && r.Method == "GET" {
		v, err := h.store.Members()
		return 200, map[string]any{"members": v}, err
	}
	return 0, nil, httpapi.NewError(404, "接口不存在")
}
