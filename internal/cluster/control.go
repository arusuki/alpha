package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/agent"
	"project-alpha/internal/bastion"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

type Control struct {
	identity        string
	nodeMu          sync.Mutex
	nodesClosed     bool
	registryMu      sync.Mutex
	registryLinks   map[string]*registryLink
	Bastion         *bastion.Handler
	memberGates     sync.Map
	provisionWake   chan struct{}
	provisionCancel context.CancelFunc
	provisionDone   chan struct{}
	lockfile        *os.File
	DB              *platform.Database
	Members         *members.Handler
	client          *http.Client
	transport       *http.Transport
	agentMu         sync.Mutex
	agents          map[string]*agent.Handler
	gates           map[string]*sync.Mutex
	closed          bool
}

func NewControl(db *platform.Database) (*Control, error) {
	lock, err := db.LockService()
	if err != nil {
		return nil, err
	}
	if err := agent.Recover(db); err != nil {
		lock.Close()
		return nil, err
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 50 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 128, MaxIdleConnsPerHost: 8}
	h := &Control{DB: db, lockfile: lock, agents: map[string]*agent.Handler{}, gates: map[string]*sync.Mutex{}, Members: members.NewHandler(db), transport: transport, client: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := h.initProvision(); err != nil {
		transport.CloseIdleConnections()
		lock.Close()
		return nil, err
	}
	if err := h.initRegistryLinks(); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}
func (h *Control) Close() {
	h.agentMu.Lock()
	if h.closed {
		h.agentMu.Unlock()
		return
	}
	h.closed = true
	handlers := make([]*agent.Handler, 0, len(h.agents))
	for _, handler := range h.agents {
		handlers = append(handlers, handler)
	}
	h.agentMu.Unlock()
	h.closeRegistryLinks()
	for _, handler := range handlers {
		handler.Manager.Close()
	}
	if h.provisionCancel != nil {
		h.provisionCancel()
		<-h.provisionDone
	}
	h.transport.CloseIdleConnections()
	h.lockfile.Close()
}

func authenticate(r *http.Request, n Node, user platform.User) {
	r.Header.Set("Authorization", "Bearer "+n.Token)
	r.Header.Set("X-Alpha-Node", n.ID)
	raw, _ := json.Marshal(user)
	r.Header.Set("X-Alpha-User", string(raw))
}
func (h *Control) probe(ctx context.Context, n Node) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info Info
	path, protocol := "/api/worker/info", Protocol
	if n.Kind == "registry" {
		path, protocol = registry.InfoPath, registry.Protocol
	}
	err := h.call(ctx, n, "GET", path, nil, platform.User{}, &info)
	if err == nil && (info.Mode != n.Kind || info.Protocol != protocol || !identifier.MatchString(info.ID)) {
		err = httpapi.NewError(409, "目标不是支持当前协议的 "+n.Kind)
	}
	return info, err
}
func (h *Control) DispatchPublic(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if status, v, e := h.statusPublic(w, r); status != 0 || e != nil {
		return status, v, e
	}
	if status, v, e := h.memberPublic(w, r); status != 0 || e != nil {
		return status, v, e
	}
	return h.Members.DispatchPublic(w, r)
}

func (h *Control) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if r.URL.Path == "/api/agent/settings" {
		return agent.NewHandler(agent.NewStore(h.DB), nil).Dispatch(w, r, user)
	}
	if bastion.IsRoute(r.URL.Path) {
		return h.Bastion.Dispatch(w, r, user)
	}
	if id, action, ok := memberAdminRoute(r.URL.Path); ok {
		if user.Role != "admin" {
			return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
		}
		return h.dispatchMemberResource(w, r, id, action, true, user.Username)
	}
	if members.IsRoute(r.URL.Path) {
		return h.Members.Dispatch(w, r, user)
	}
	fail := func(err error) (int, any, error) { return 0, nil, err }
	if r.Method != "GET" && user.Role != "admin" {
		return fail(httpapi.NewError(403, "此操作需要管理员权限"))
	}
	if r.URL.Path == "/api/cluster/overview" && r.Method == "GET" {
		return h.overview(r, user)
	}
	if r.URL.Path == "/api/cluster/nodes" {
		if r.Method == "GET" {
			nodes, err := h.nodes("")
			return 200, map[string]any{"nodes": nodes}, err
		}
		if r.Method == "POST" {
			return h.saveNode(w, r, user, "")
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/cluster/nodes/"), "/")
	if strings.HasPrefix(r.URL.Path, "/api/cluster/nodes/") && len(parts) > 0 && identifier.MatchString(parts[0]) {
		n, err := h.node(parts[0])
		if err != nil {
			return fail(err)
		}
		if len(parts) == 1 {
			switch r.Method {
			case "GET":
				return 200, n, nil
			case "PUT":
				return h.saveNode(w, r, user, n.ID)
			case "DELETE":
				h.nodeMu.Lock()
				defer h.nodeMu.Unlock()
				if h.nodesClosed {
					return fail(httpapi.NewError(503, "总控正在关闭"))
				}
				err = h.DB.Transaction(func(tx *sql.Tx) error {
					var count int
					if err := tx.QueryRow("SELECT count(*) FROM member_node_resources WHERE node_id=?", n.ID).Scan(&count); err != nil {
						return err
					}
					if count > 0 {
						return httpapi.NewError(409, "节点仍有使用者资源记录，请先回收关联使用者资源")
					}
					if _, err := tx.Exec("DELETE FROM cluster_nodes WHERE id=?", n.ID); err != nil {
						return err
					}
					return platform.Audit(tx, user.Username, "cluster.node.remove", n.Name+" / "+n.ID)
				})
				if err == nil && n.Kind == "registry" {
					h.stopRegistry(n.ID)
				}
				return 200, map[string]bool{"ok": true}, err
			}
		}
		if len(parts) > 2 && parts[1] == "api" {
			if n.Kind != "worker" {
				return fail(httpapi.NewError(404, "registry 不提供计算节点操作"))
			}
			path := "/" + strings.Join(parts[1:], "/")
			if agent.IsRoute(path) {
				return h.dispatchAgent(w, r, user, n, path)
			}
			if r.Method == "DELETE" && strings.HasPrefix(path, "/api/jobs/") && identifier.MatchString(strings.TrimPrefix(path, "/api/jobs/")) {
				return h.deleteRecord(r, user, n, strings.TrimPrefix(path, "/api/jobs/"))
			}
			if strings.HasPrefix(path, "/api/containers/members/") {
				return fail(httpapi.NewError(403, "请使用使用者资源 API"))
			}
			if !operational(path) {
				return fail(httpapi.NewError(404, "此接口不属于节点操作 API"))
			}
			if err := h.validateOwner(w, r, path); err != nil {
				return fail(err)
			}
			h.proxy(w, r, n, path, user)
			return 0, nil, nil
		}
	}
	return fail(httpapi.NewError(404, "接口不存在，请先选择节点"))
}

func (h *Control) saveNode(w http.ResponseWriter, r *http.Request, user platform.User, id string) (int, any, error) {
	h.nodeMu.Lock()
	defer h.nodeMu.Unlock()
	if h.nodesClosed {
		return 0, nil, httpapi.NewError(503, "总控正在关闭")
	}
	var value nodeInput
	var previous Node
	if err := httpapi.DecodeBody(w, r, &value); err != nil {
		return 0, nil, err
	}
	if id != "" {
		n, err := h.node(id)
		if err != nil {
			return 0, nil, err
		}
		if value.Kind != n.Kind {
			return 0, nil, httpapi.NewError(400, "不能更改已有节点的类型，请添加新节点")
		}
		if value.Token == "" {
			value.Token = n.Token
		}
		previous = n
	}
	if err := value.validate(); err != nil {
		return 0, nil, err
	}
	n := Node{Kind: value.Kind, ID: id, Name: value.Name, URL: value.URL, Token: value.Token, CreatedAt: platform.Now()}
	info, err := h.probe(r.Context(), n)
	if err != nil {
		return 0, nil, err
	}
	if id != "" && info.ID != id {
		return 0, nil, httpapi.NewError(409, "地址对应另一个节点；请将其作为新节点添加")
	}
	n.ID = info.ID
	err = h.DB.Transaction(func(tx *sql.Tx) error {
		var err error
		action := "cluster.node.update"
		if id == "" {
			var count int
			if err = tx.QueryRow("SELECT count(*) FROM cluster_nodes").Scan(&count); err != nil {
				return err
			}
			if count >= 128 {
				return httpapi.NewError(409, "最多可添加 128 个节点")
			}
			_, err = tx.Exec("INSERT INTO cluster_nodes VALUES(?,?,?,?,?,?)", n.ID, n.Name, n.URL, n.Token, n.CreatedAt, n.Kind)
			action = "cluster.node.add"
		} else {
			result, e := tx.Exec("UPDATE cluster_nodes SET name=?,url=?,token=? WHERE id=?", n.Name, n.URL, n.Token, id)
			err = e
			if err == nil {
				count, e := result.RowsAffected()
				if e != nil {
					return e
				}
				if count != 1 {
					return httpapi.NewError(404, "节点已移除")
				}
			}
		}
		if platform.IsConstraint(err) {
			return httpapi.NewError(409, "该节点或地址已经添加")
		}
		if err != nil {
			return err
		}
		return platform.Audit(tx, user.Username, action, n.Name+" / "+n.ID)
	})
	if err != nil {
		return 0, nil, err
	}
	n, err = h.node(n.ID)
	if err == nil && n.Kind == "registry" && (id == "" || n.URL != previous.URL || n.Token != previous.Token) {
		h.startRegistry(n)
	}
	status := 200
	if id == "" {
		status = 201
	}
	return status, n, err
}

// Owners are stable central member usernames, not platform login accounts.
func (h *Control) validateOwner(w http.ResponseWriter, r *http.Request, path string) error {
	if !(path == "/api/containers" && r.Method == "POST") && !(path == "/api/owners" && r.Method == "PUT") {
		return nil
	}
	value, err := httpapi.RequestBody(w, r)
	if err != nil {
		return err
	}
	owner := httpapi.FieldString(value, "owner")
	if owner != "" {
		var count int
		if err = h.DB.SQL.QueryRow("SELECT count(*) FROM members WHERE username=? AND status='active'", owner).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return httpapi.NewError(400, "所属用户必须是总控已登记的使用者标识")
		}
	} else if path == "/api/containers" {
		return httpapi.NewError(400, "请选择总控已登记的所属用户")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return nil
}

func (h *Control) proxy(w http.ResponseWriter, r *http.Request, n Node, path string, user platform.User) {
	target, _ := url.Parse(n.URL)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Streaming and slow requests lose access when the central session or node
	// registration is revoked; reconnects always pass through platform auth.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				session, err := h.DB.Session(httpapi.SessionToken(r))
				current, nodeErr := h.node(n.ID)
				if err != nil || session.User != user || nodeErr != nil || current.URL != n.URL || current.Token != n.Token {
					cancel()
					return
				}
			}
		}
	}()
	proxy := httputil.ReverseProxy{Transport: h.transport, FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = target.Scheme
			p.Out.URL.Host = target.Host
			p.Out.URL.Path = path
			p.Out.URL.RawPath = ""
			p.Out.Host = target.Host
			// Only operational headers cross the trust boundary.
			p.Out.Header = make(http.Header)
			for _, key := range []string{"Content-Type", "If-None-Match", "Last-Event-ID", "Accept"} {
				if v := p.In.Header.Get(key); v != "" {
					p.Out.Header.Set(key, v)
				}
			}
			authenticate(p.Out, n, user)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("worker authentication rejected")
			}
			resp.Header.Del("Location")
			if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != 304 {
				return fmt.Errorf("worker redirect rejected")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, httpapi.NewError(502, "节点请求失败，请检查节点连接；修改操作请刷新核实结果后再重试"))
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}
