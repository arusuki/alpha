package registry

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

//go:embed page.html app.js style.css
var assets embed.FS
var page = template.Must(template.ParseFS(assets, "page.html"))
var passPattern = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)
var entryPattern = regexp.MustCompile(`^/registry/([A-Za-z0-9]{8})/([a-f0-9]{48})(/.*)?$`)

func ValidPass(pass string) bool { return passPattern.MatchString(pass) }

type session struct {
	Hash, CSRF, Token, Schema, Registration string
	Registered                              bool
	Expires                                 float64
}
type attempt struct {
	Count int
	Since time.Time
}
type Server struct {
	DB       *platform.Database
	Hub      *Hub
	Pass     string
	Secure   bool
	hosts    map[string]bool
	mu       sync.Mutex
	attempts map[string]attempt
	streams  chan struct{}
}

func NewServer(db *platform.Database, pass, token string, hosts []string, secure bool) *Server {
	s := &Server{DB: db, Hub: &Hub{DB: db, Token: token, Pass: pass}, Pass: pass, Secure: secure,
		hosts: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}, attempts: map[string]attempt{}, streams: make(chan struct{}, 128)}
	for _, host := range hosts {
		s.hosts[strings.ToLower(host)] = true
	}
	return s
}

func (s *Server) allow(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, a := range s.attempts {
		if now.Sub(a.Since) >= 5*time.Minute {
			delete(s.attempts, key)
		}
	}
	a, exists := s.attempts[host]
	if a.Count >= 60 || !exists && len(s.attempts) >= 4096 {
		return false
	}
	if !exists {
		a.Since = now
	}
	a.Count++
	s.attempts[host] = a
	return true
}

func (s *Server) session(r *http.Request, invitation string) (*session, error) {
	cookie, err := r.Cookie("alpha_registry")
	if err != nil || len(cookie.Value) != 64 {
		return nil, sql.ErrNoRows
	}
	v := &session{Hash: digest(cookie.Value)}
	err = s.DB.SQL.QueryRow(`SELECT csrf,resource_token,schema_json,registration,registered,expires_at FROM registry_sessions WHERE token_hash=? AND invitation_hash=? AND expires_at>?`, v.Hash, digest(invitation), platform.Now()).Scan(&v.CSRF, &v.Token, &v.Schema, &v.Registration, &v.Registered, &v.Expires)
	return v, err
}

func (s *Server) createSession(w http.ResponseWriter, base, invitation string, schema json.RawMessage) (*session, error) {
	value := platform.RandomHex(32)
	v := &session{Hash: digest(value), CSRF: platform.RandomHex(32), Token: platform.RandomHex(32), Schema: string(schema), Expires: platform.Now() + 7*86400}
	err := s.DB.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM registry_sessions WHERE expires_at<=?", platform.Now()); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM registry_sessions").Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return httpapi.NewError(503, "注册会话已满，请稍后重试")
		}
		_, err := tx.Exec("INSERT INTO registry_sessions(token_hash,invitation_hash,csrf,resource_token,schema_json,expires_at) VALUES(?,?,?,?,?,?)", v.Hash, digest(invitation), v.CSRF, v.Token, v.Schema, v.Expires)
		return err
	})
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{Name: "alpha_registry", Value: value, Path: base, MaxAge: 7 * 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.Secure})
	return v, nil
}

func writeError(w http.ResponseWriter, err error) {
	status, message := 500, "注册服务内部错误，请联系管理员"
	var api *httpapi.Error
	if errors.As(err, &api) {
		status, message = api.Status, api.Message
	}
	httpapi.WriteJSON(w, status, map[string]string{"error": message})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, err := url.Parse("//" + r.Host)
	if err != nil || host.User != nil || host.Path != "" || !s.hosts[strings.ToLower(host.Hostname())] {
		panic(http.ErrAbortHandler)
	}
	if (r.URL.Path == LinkPath || r.URL.Path == InfoPath) && r.URL.RawQuery == "" && r.URL.RawPath == "" {
		s.Hub.ServeHTTP(w, r)
		return
	}
	match := entryPattern.FindStringSubmatch(r.URL.Path)
	if match == nil || r.URL.RawPath != "" || r.URL.RawQuery != "" || !hmac.Equal([]byte(match[1]), []byte(s.Pass)) {
		panic(http.ErrAbortHandler)
	}
	base := "/registry/" + match[1] + "/" + match[2]
	action := match[3]
	if !((action == "" || action == "/" || action == "/app.js" || action == "/style.css" || action == "/api/session" || action == "/api/events") && r.Method == "GET" || (action == "/api/register" || action == "/api/retry") && r.Method == "POST") {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && action != "" && action != "/" {
		panic(http.ErrAbortHandler)
	}
	if r.Method == "POST" {
		origin, e := url.Parse(r.Header.Get("Origin"))
		if e != nil || origin.Host != r.Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "http" && origin.Scheme != "https") {
			panic(http.ErrAbortHandler)
		}
	}
	v, err := s.session(r, match[2])
	if err != nil && err != sql.ErrNoRows {
		writeError(w, err)
		return
	}
	if action == "" || action == "/" {
		if err == sql.ErrNoRows || v.Registration == "" {
			if !s.allow(r) {
				panic(http.ErrAbortHandler)
			}
			schema, e := s.Hub.Call(r.Context(), Request{Action: "validate", Invitation: match[2]})
			if e != nil {
				var api *httpapi.Error
				if errors.As(e, &api) && api.Status == 400 {
					panic(http.ErrAbortHandler)
				}
				writeError(w, e)
				return
			}
			if err == sql.ErrNoRows {
				v, err = s.createSession(w, base, match[2], schema)
			} else {
				_, err = s.DB.SQL.Exec("UPDATE registry_sessions SET schema_json=? WHERE token_hash=?", string(schema), v.Hash)
			}
			if err != nil {
				writeError(w, err)
				return
			}
		}
		var body bytes.Buffer
		if err = page.Execute(&body, struct{ Base string }{base}); err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body.Bytes())
		return
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if r.Method == "POST" && !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(v.CSRF)) {
		panic(http.ErrAbortHandler)
	}
	switch action {
	case "/app.js", "/style.css":
		body, err := assets.ReadFile(strings.TrimPrefix(action, "/"))
		if err != nil {
			writeError(w, err)
			return
		}
		typ := "text/javascript; charset=utf-8"
		if action == "/style.css" {
			typ = "text/css; charset=utf-8"
		}
		w.Header().Set("Content-Type", typ)
		w.Write(body)
	case "/api/session":
		value := map[string]any{"schema": json.RawMessage(v.Schema), "csrf": v.CSRF, "submitted": v.Registration != "", "registered": v.Registered}
		if v.Registered {
			value["resource_token"] = v.Token
		}
		httpapi.WriteJSON(w, 200, value)
	case "/api/register":
		if !s.allow(r) {
			writeError(w, httpapi.NewError(429, "请求过多，请在 5 分钟后重试"))
			return
		}
		if err = s.register(w, r, v, match[2]); err != nil {
			writeError(w, err)
			return
		}
		httpapi.WriteJSON(w, 200, map[string]any{"ok": true, "resource_token": v.Token})
	case "/api/retry":
		if !v.Registered || !s.allow(r) {
			writeError(w, httpapi.NewError(409, "请等待当前注册完成后再重试资源分配"))
			return
		}
		_, err = s.Hub.Call(r.Context(), Request{Action: "retry", Token: v.Token})
		if err != nil {
			writeError(w, err)
			return
		}
		httpapi.WriteJSON(w, 200, map[string]bool{"ok": true})
	case "/api/events":
		if !v.Registered {
			writeError(w, httpapi.NewError(409, "请先提交注册"))
			return
		}
		s.events(w, r, v)
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request, v *session, invitation string) error {
	input, err := httpapi.RequestBody(w, r)
	if err != nil {
		return err
	}
	if v.Registered {
		return nil
	}
	if v.Registration == "" {
		raw, _ := json.Marshal(input)
		var req members.Registration
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(&req); err != nil || req.InvitationCode != "" {
			return httpapi.NewError(400, "注册字段无效；邀请码由入口提供")
		}
		body, _ := json.Marshal(req)
		// The compare-and-set makes simultaneous submissions use the same request.
		if _, err = s.DB.SQL.Exec("UPDATE registry_sessions SET registration=? WHERE token_hash=? AND registration=''", string(body), v.Hash); err != nil {
			return err
		}
		if err = s.DB.SQL.QueryRow("SELECT registration FROM registry_sessions WHERE token_hash=?", v.Hash).Scan(&v.Registration); err != nil {
			return err
		}
	}
	// The invitation stays in the entry URL; only its digest is persisted here.
	var registration members.Registration
	if err = json.Unmarshal([]byte(v.Registration), &registration); err != nil {
		return err
	}
	registration.InvitationCode = invitation
	body, err := json.Marshal(registration)
	if err != nil {
		return err
	}
	_, err = s.Hub.Call(r.Context(), Request{Action: "register", Invitation: invitation, Token: v.Token, Body: body})
	if err != nil {
		var api *httpapi.Error
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
			_, clearErr := s.DB.SQL.Exec("UPDATE registry_sessions SET registration='' WHERE token_hash=? AND registered=0 AND registration=?", v.Hash, v.Registration)
			if clearErr != nil {
				return clearErr
			}
		}
		return err
	}
	_, err = s.DB.SQL.Exec("UPDATE registry_sessions SET registered=1 WHERE token_hash=?", v.Hash)
	return err
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, v *session) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		writeError(w, httpapi.NewError(503, "进度连接已满，请稍后重试"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for platform.Now() < v.Expires {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		body, err := s.Hub.Call(ctx, Request{Action: "resources", Token: v.Token})
		cancel()
		event := "progress"
		if err != nil {
			event = "problem"
			body, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); e != nil {
			return
		}
		if e := controller.Flush(); e != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
