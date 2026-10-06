package registry

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const LinkPath = "/api/registry/connect"
const InfoPath = "/api/registry/info"
const Protocol = 3
const protocol = "alpha-registry-v3"

var identityPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Request struct {
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	Invitation string          `json:"invitation,omitempty"`
	Token      string          `json:"token,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}
type Response struct {
	ID     string          `json:"id"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Error  string          `json:"error,omitempty"`
}
type Dispatch func(context.Context, Request) (any, error)

type LinkConfig struct {
	Address, Token, ControlID, RegistryID string
}
type LinkStatus struct {
	State       string  `json:"state"`
	Error       string  `json:"error,omitempty"`
	ConnectedAt float64 `json:"connected_at,omitempty"`
	LastSeen    float64 `json:"last_seen,omitempty"`
}

func response(req Request, body any, err error) Response {
	out := Response{ID: req.ID, Status: 200}
	if err == nil {
		out.Body, err = json.Marshal(body)
	}
	if err != nil {
		out.Status, out.Error = 500, "control 内部错误，请联系管理员"
		var api *httpapi.Error
		if errors.As(err, &api) {
			out.Status, out.Error = api.Status, api.Message
		} else {
			log.Printf("registry operation %s: %v", req.Action, err)
		}
	}
	return out
}

// Endpoint accepts only origins, never caller-specified paths or credentials.
// Public endpoints must use TLS; plaintext is limited to local development.
func Endpoint(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("registry URL must be an https:// origin without a path or credentials")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
			return "", fmt.Errorf("public registry URL requires HTTPS")
		}
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("registry URL must use https (http only on loopback)")
	}
	u.Path = LinkPath
	return u.String(), nil
}

// Connect runs on control. Every registry gets its own outbound, reconnecting link.
func Connect(ctx context.Context, link LinkConfig, dispatch Dispatch, observe func(LinkStatus)) {
	status := LinkStatus{State: "connecting"}
	publish := func() {
		if observe != nil {
			observe(status)
		}
	}
	publish()
	endpoint, err := Endpoint(link.Address)
	if err != nil {
		log.Printf("registry: %v", err)
		return
	}
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		config, _ := websocket.NewConfig(endpoint, link.Address)
		config.Protocol = []string{protocol}
		config.Header.Set("Authorization", "Bearer "+link.Token)
		config.Header.Set("X-Alpha-Control", link.ControlID)
		config.Header.Set("X-Alpha-Registry", link.RegistryID)
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := config.DialContext(dialCtx)
		cancel()
		if err == nil {
			log.Printf("registry connected: %s", link.Address)
			status = LinkStatus{State: "connected", ConnectedAt: platform.Now()}
			err = serveControl(ctx, conn, dispatch, func() {
				status.LastSeen = platform.Now()
				publish()
			})
		}
		if ctx.Err() != nil {
			return
		}
		status.State = "reconnecting"
		status.Error = "连接中断或认证失败，请检查地址、令牌与 registry 绑定；正在自动重连"
		publish()
		log.Printf("registry disconnected: %s (%v); reconnecting", link.Address, err)
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func serveControl(ctx context.Context, conn *websocket.Conn, dispatch Dispatch, heartbeat func()) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.MaxPayloadBytes = 1 << 20
	for {
		conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		var req Request
		if err := websocket.JSON.Receive(conn, &req); err != nil {
			return err
		}
		if !identityPattern.MatchString(req.ID) {
			return fmt.Errorf("invalid registry request ID")
		}
		var body any
		var err error
		if req.Action == "ping" {
			body = map[string]bool{"ok": true}
		} else {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			body, err = dispatch(callCtx, req)
			cancel()
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := websocket.JSON.Send(conn, response(req, body, err)); err != nil {
			return err
		}
		heartbeat()
	}
}

type peer struct {
	conn *websocket.Conn
	gate chan struct{}
	done chan struct{}
	once sync.Once
}

func (p *peer) close() { p.once.Do(func() { close(p.done); p.conn.Close() }) }
func unavailable() error {
	return httpapi.NewError(503, "control 连接暂时不可用，正在等待重连")
}
func (p *peer) call(ctx context.Context, req Request) (json.RawMessage, error) {
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, unavailable()
	case <-p.done:
		return nil, unavailable()
	}
	defer func() { <-p.gate }()
	select {
	case <-p.done:
		return nil, unavailable()
	default:
	}
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	p.conn.SetDeadline(deadline)
	req.ID = platform.RandomHex(16)
	if err := websocket.JSON.Send(p.conn, req); err != nil {
		p.close()
		return nil, unavailable()
	}
	var out Response
	if err := websocket.JSON.Receive(p.conn, &out); err != nil || out.ID != req.ID {
		p.close()
		return nil, unavailable()
	}
	if out.Status != 200 {
		if out.Status < 400 || out.Status > 599 {
			p.close()
			return nil, unavailable()
		}
		return nil, httpapi.NewError(out.Status, out.Error)
	}
	return out.Body, nil
}

type Hub struct {
	DB     *platform.Database
	Token  string
	Pass   string
	mu     sync.Mutex
	peer   *peer
	closed bool
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.peer != nil {
		h.peer.close()
	}
}
func (h *Hub) Call(ctx context.Context, req Request) (json.RawMessage, error) {
	h.mu.Lock()
	p := h.peer
	h.mu.Unlock()
	if p == nil {
		return nil, unavailable()
	}
	return p.call(ctx, req)
}
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Alpha-Control")
	if r.Method != "GET" || h.Token == "" || !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.Token)) || !identityPattern.MatchString(id) {
		panic(http.ErrAbortHandler)
	}
	registryID, err := h.DB.CheckMode("registry")
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Path == InfoPath {
		var bound string
		err := h.DB.SQL.QueryRow("SELECT control_id FROM registry_control WHERE id=1").Scan(&bound)
		if err != nil && err != sql.ErrNoRows {
			writeError(w, err)
			return
		}
		if bound != "" && bound != id {
			writeError(w, httpapi.NewError(409, "registry 已绑定其他 control"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpapi.WriteJSON(w, 200, map[string]any{"id": registryID, "mode": "registry", "protocol": Protocol, "registration_path": "/registry/" + h.Pass + "/"})
		return
	}
	if r.Header.Get("Sec-WebSocket-Protocol") != protocol || r.Header.Get("X-Alpha-Registry") != registryID {
		panic(http.ErrAbortHandler)
	}
	server := websocket.Server{
		Handshake: func(config *websocket.Config, _ *http.Request) error {
			if err := bindControl(h.DB, id); err != nil {
				return err
			}
			config.Protocol = []string{protocol}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = 1 << 20
			p := &peer{conn: conn, gate: make(chan struct{}, 1), done: make(chan struct{})}
			defer p.close()
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			if h.peer != nil {
				h.peer.close()
			}
			h.peer = p
			h.mu.Unlock()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				if _, err := p.call(context.Background(), Request{Action: "ping"}); err != nil {
					return
				}
				select {
				case <-p.done:
					return
				case <-ticker.C:
				}
			}
		},
	}
	server.ServeHTTP(w, r)
}
