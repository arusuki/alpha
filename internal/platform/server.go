package platform

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
)

type loginAttempt struct {
	Count int
	At    time.Time
}
type Server struct {
	DB           *Database
	Module       Module
	Assets       fs.ReadFileFS
	AllowedHosts map[string]bool
	SecureCookie bool
	mu           sync.Mutex
	attempts     map[string]loginAttempt
}

// Module handles authenticated requests after the platform's host and CSRF checks.
type Module interface {
	Dispatch(http.ResponseWriter, *http.Request, User) (int, any, error)
}

// PublicModule handles explicitly public routes after Host and Origin validation.
// A zero status and nil error leave the request to the authenticated dispatcher.
type PublicModule interface {
	DispatchPublic(http.ResponseWriter, *http.Request) (int, any, error)
}

func NewServer(db *Database, module Module, assets fs.ReadFileFS, hosts []string, secure bool) *Server {
	s := &Server{DB: db, Module: module, Assets: assets, AllowedHosts: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}, SecureCookie: secure, attempts: map[string]loginAttempt{}}
	for _, h := range hosts {
		s.AllowedHosts[strings.ToLower(h)] = true
	}
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; worker-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	defer func() {
		if p := recover(); p != nil {
			log.Printf("HTTP panic: %v", p)
			httpapi.WriteJSON(w, 500, object{"error": "服务内部错误，请检查服务日志"})
		}
	}()
	if err := s.guard(r); err != nil {
		s.writeError(w, err)
		return
	}
	assets := map[string]string{
		"/": "index.html", "/usage.js": "usage.js", "/snapshot.js": "snapshot.js",
		"/snapshot-loader.js": "snapshot-loader.js", "/snapshot-worker.js": "snapshot-worker.js",
		"/snapshot-cache.js": "snapshot-cache.js",
		"/app.js":            "app.js", "/platform.js": "platform.js", "/settings.js": "settings.js",
		"/agent.js": "agent.js", "/cleanup.js": "cleanup.js", "/dashboard.js": "dashboard.js", "/process.js": "process.js", "/containers.js": "containers.js",
		"/style.css": "style.css", "/workspace.css": "workspace.css",
		"/cluster.js": "cluster.js", "/cluster.css": "cluster.css", "/auth.css": "auth.css", "/auth.js": "auth.js", "/members.js": "members.js", "/bastion.js": "bastion.js",
	}
	assetPath := r.URL.Path
	if nodePageRoute.MatchString(assetPath) {
		assetPath = "/"
	}
	assets["/status.js"] = "status.js"
	assets["/status.css"] = "status.css"
	if statusPageRoute.MatchString(assetPath) {
		assets[assetPath] = "status.html"
	}
	if filename, ok := assets[assetPath]; r.Method == "GET" && ok {
		body, err := s.Assets.ReadFile(filename)
		if err != nil {
			s.writeError(w, err)
			return
		}
		typ := mime.TypeByExtension(filepath.Ext(filename))
		w.Header().Set("Content-Type", typ)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
		return
	}
	status, value, err := s.dispatch(w, r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if status != 0 {
		httpapi.WriteJSON(w, status, value)
	}
}
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var api *httpapi.Error
	if errors.As(err, &api) {
		httpapi.WriteJSON(w, api.Status, object{"error": api.Message})
	} else {
		log.Printf("HTTP: %v", err)
		httpapi.WriteJSON(w, 500, object{"error": "服务内部错误，请检查服务日志"})
	}
}
func (s *Server) guard(r *http.Request) error {
	host, err := url.Parse("//" + r.Host)
	if err != nil || host.Host == "" || host.User != nil || host.Path != "" {
		return httpapi.NewError(400, "无效的 Host")
	}
	if !s.AllowedHosts[strings.ToLower(host.Hostname())] {
		address, err := netip.ParseAddr(host.Hostname())
		if err != nil {
			return httpapi.NewError(403, "该访问域名未在服务配置中允许")
		}
		ip := address.Unmap().String()
		var count int
		port := host.Port()
		if s.DB.SQL.QueryRow("SELECT count(*) FROM bastion_tailscale WHERE ssh_host=? AND CAST(status_port AS TEXT)=?", ip, port).Scan(&count) != nil || count == 0 {
			return httpapi.NewError(403, "该访问域名未在服务配置中允许")
		}
	}
	if r.Method != "GET" {
		origin := r.Header.Get("Origin")
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			return httpapi.NewError(403, "不允许跨站修改请求")
		}
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return httpapi.NewError(403, "不允许跨站修改请求")
			}
		}
	}
	return nil
}
func (s *Server) checkAttempts(r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for ip, a := range s.attempts {
		if now.Sub(a.At) > 5*time.Minute {
			delete(s.attempts, ip)
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	a := s.attempts[ip]
	if a.Count >= 10 || len(s.attempts) > 1024 {
		return httpapi.NewError(429, "登录尝试过多，请在 5 分钟后重试")
	}
	s.attempts[ip] = loginAttempt{a.Count + 1, now}
	return nil
}
func (s *Server) cookie(w http.ResponseWriter, token string) {
	maxAge := 43200
	if token == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "project_alpha_session", Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.SecureCookie})
}

var nodePageRoute = regexp.MustCompile(`^/nodes/[a-f0-9]{32}/$`)
var statusPageRoute = regexp.MustCompile(`^/status/[a-z][a-z0-9_-]{2,31}/?$`)

var userRoute = regexp.MustCompile(`^/api/users/([a-f0-9]{32})$`)

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) (int, any, error) {
	route, method := r.URL.Path, r.Method
	db := s.DB
	failure := func(err error) (int, any, error) { return 0, nil, err }
	if method == "GET" && route == "/api/session" {
		configured, err := db.Configured()
		if err != nil {
			return failure(err)
		}
		if !configured {
			return 200, object{"setup_required": true}, nil
		}
		session, err := db.Session(httpapi.SessionToken(r))
		if err != nil {
			var api *httpapi.Error
			if errors.As(err, &api) && api.Status == 401 {
				return 200, object{"setup_required": false, "user": nil}, nil
			}
			return failure(err)
		}
		return 200, object{"setup_required": false, "user": session.User, "csrf": session.CSRF}, nil
	}
	if method == "POST" && (route == "/api/setup" || route == "/api/login") {
		value, err := httpapi.RequestBody(w, r)
		if err != nil {
			return failure(err)
		}
		if err = s.checkAttempts(r); err != nil {
			return failure(err)
		}
		if route == "/api/setup" {
			if _, err = db.CreateUser(value, "setup", true); err != nil {
				return failure(err)
			}
		}
		var name, password string
		if json.Unmarshal(value["username"], &name) != nil || json.Unmarshal(value["password"], &password) != nil || string(value["username"]) == "null" || string(value["password"]) == "null" {
			return failure(httpapi.NewError(400, "账号或密码格式无效"))
		}
		token, session, err := db.Login(name, password)
		if err != nil {
			return failure(err)
		}
		s.mu.Lock()
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		delete(s.attempts, ip)
		s.mu.Unlock()
		s.cookie(w, token)
		return 200, object{"user": session.User, "csrf": session.CSRF}, nil
	}
	if public, ok := s.Module.(PublicModule); ok {
		status, value, err := public.DispatchPublic(w, r)
		if status != 0 || err != nil {
			return status, value, err
		}
	}
	session, err := db.Session(httpapi.SessionToken(r))
	if err != nil {
		return failure(err)
	}
	user := session.User
	admin := user.Role == "admin"
	if method != "GET" {
		if !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) {
			return failure(httpapi.NewError(403, "会话校验失败，请重新登录"))
		}
		if route != "/api/logout" && route != "/api/password" && !admin {
			return failure(httpapi.NewError(403, "此操作需要管理员权限"))
		}
	}
	if (route == "/api/users" || route == "/api/audit") && !admin {
		return failure(httpapi.NewError(403, "此操作需要管理员权限"))
	}
	if method == "POST" && (route == "/api/logout" || route == "/api/password") {
		value, err := httpapi.RequestBody(w, r)
		if err != nil {
			return failure(err)
		}
		if route == "/api/logout" {
			err = db.Logout(httpapi.SessionToken(r))
		} else {
			err = db.ChangePassword(user.ID, httpapi.FieldString(value, "old_password"), httpapi.FieldString(value, "new_password"))
		}
		if err != nil {
			return failure(err)
		}
		s.cookie(w, "")
		return 200, object{"ok": true}, nil
	}
	if route == "/api/users" {
		if method == "GET" {
			users, err := db.Users()
			return 200, object{"users": users}, err
		}
		if method == "POST" {
			value, err := httpapi.RequestBody(w, r)
			if err != nil {
				return failure(err)
			}
			user, err := db.CreateUser(value, user.Username, false)
			return 201, user, err
		}
	}
	if match := userRoute.FindStringSubmatch(route); method == "PATCH" && match != nil {
		value, err := httpapi.RequestBody(w, r)
		if err != nil {
			return failure(err)
		}
		err = db.UpdateUser(match[1], value, user.Username)
		return 200, object{"ok": true}, err
	}
	if method == "GET" && route == "/api/audit" {
		events, err := db.AuditRows()
		return 200, object{"events": events}, err
	}
	return s.Module.Dispatch(w, r, user)
}
