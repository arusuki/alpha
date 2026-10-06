package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

type moduleFunc func(http.ResponseWriter, *http.Request, platform.User) (int, any, error)

func (f moduleFunc) Dispatch(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	return f(w, r, u)
}

type fixture struct {
	db          *platform.Database
	control     *Control
	server      *platform.Server
	user        platform.User
	token, csrf string
}

func TestControlSettingsRoutesRemoved(t *testing.T) {
	f := setup(t)
	requireStatus(t, f.request(t, "GET", "/api/control/settings", nil), 404)
	requireStatus(t, f.request(t, "PUT", "/api/control/settings", map[string]any{}), 404)
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CheckMode("control"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	_, err = db.CreateUser(map[string]json.RawMessage{"username": json.RawMessage(`"admin"`), "password": json.RawMessage(`"password-123456"`)}, "setup", true)
	if err != nil {
		t.Fatal(err)
	}
	token, session, err := db.Login("admin", "password-123456")
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewControl(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(control.Close)
	server := platform.NewServer(db, control, web.Assets, nil, false)

	return &fixture{db, control, server, session.User, token, session.CSRF}
}
func (f *fixture) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.csrf)
	r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: f.token})
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d: %s", w.Code, status, w.Body.String())
	}
}
func worker(t *testing.T, id string, inventory Inventory, module platform.Module) (*Worker, *httptest.Server) {
	t.Helper()
	w := &Worker{ID: id, Token: strings.Repeat("secret", 8), Module: module, Inventory: func() (Inventory, error) { return inventory, nil }}
	s := httptest.NewServer(w)
	t.Cleanup(s.Close)
	return w, s
}
func add(t *testing.T, f *fixture, w *Worker, s *httptest.Server, name string) {
	t.Helper()
	response := f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": "10.0.0.11", "name": name, "url": s.URL, "token": w.Token})
	requireStatus(t, response, 201)
	if strings.Contains(response.Body.String(), w.Token) {
		t.Fatal("node credential leaked")
	}
}

func TestClusterNodeLifecycleAndAggregate(t *testing.T) {
	f := setup(t)
	w1, s1 := worker(t, strings.Repeat("a", 32), Inventory{Containers: []Container{{ID: "same", Name: "train", Owner: "alice", Managed: true}, {ID: "other", Name: "unassigned"}}}, nil)
	w2, s2 := worker(t, strings.Repeat("b", 32), Inventory{Containers: []Container{{ID: "same", Name: "train", Owner: "alice", Managed: true}, {ID: "unknown", Name: "legacy", Owner: "bob"}}}, nil)
	add(t, f, w1, s1, "node-a")
	add(t, f, w2, s2, "node-b")
	memberStore := &members.Store{Database: f.db}
	invitation, err := memberStore.CreateInvitation("test", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = memberStore.RegisterWith(members.Registration{Password: "Member-password-123", SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f", Username: "alice", InvitationCode: invitation.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, nil); err != nil {
		t.Fatal(err)
	}
	response := f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, response, 200)
	var result struct {
		Nodes   []NodeStatus    `json:"nodes"`
		Members []MemberSummary `json:"members"`
		Count   int             `json:"container_count"`
		Partial bool            `json:"partial"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 4 || result.Partial || len(result.Nodes) != 2 {
		t.Fatalf("bad total: %+v", result)
	}
	for _, m := range result.Members {
		if m.Username == "alice" && (!m.Registered || m.Count != 2 || len(m.Nodes) != 2) {
			t.Fatalf("cross-node identity merged incorrectly: %+v", m)
		}
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": "10.0.0.11", "name": "duplicate", "url": s1.URL, "token": w1.Token}), 409)
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+w1.ID, map[string]string{"kind": "worker", "internal_ip": "10.0.0.11", "name": "renamed", "url": s1.URL, "token": ""}), 200)
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+w1.ID, map[string]string{"kind": "worker", "internal_ip": "10.0.0.11", "name": "wrong node", "url": s2.URL, "token": ""}), 409)
	s2.Close()
	response = f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, response, 200)
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Count != 2 || result.Nodes[1].Online || result.Nodes[1].Error == "" {
		t.Fatalf("offline node reported as complete: %+v", result)
	}
	requireStatus(t, f.request(t, "DELETE", "/api/cluster/nodes/"+w2.ID, nil), 200)
	requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+w2.ID, nil), 404)
	for _, path := range []string{"/", "/nodes/" + w1.ID + "/", "/cluster.js", "/cluster.css"} {
		requireStatus(t, f.request(t, "GET", path, nil), 200)
	}
	if _, err = f.db.CheckMode("worker"); err == nil {
		t.Fatal("control data opened as worker")
	}
	if _, err = f.db.SQL.Exec("SELECT * FROM jobs"); err == nil {
		t.Fatal("control must not initialize scan tables")
	}
}

func TestProxyIdentityPermissionsAndSecrets(t *testing.T) {
	f := setup(t)
	called := 0
	mod := moduleFunc(func(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		called++
		if u != f.user {
			t.Errorf("wrong user: %+v", u)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("Forwarded") != "" {
			t.Error("browser credentials crossed boundary")
		}
		if r.URL.RawQuery != "revision=7" || r.Header.Get("If-None-Match") != "test-etag" {
			t.Error("proxy lost request metadata")
		}
		w.Header().Set("ETag", "test-etag")
		w.WriteHeader(304)
		return 0, nil, nil
	})
	node, s := worker(t, strings.Repeat("c", 32), Inventory{Containers: []Container{}}, mod)
	add(t, f, node, s, "worker")
	path := "/api/cluster/nodes/" + node.ID + "/api/jobs/" + strings.Repeat("d", 32) + "/snapshot?revision=7"
	r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
	r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: f.token})
	r.Header.Set("Authorization", "Bearer attacker")
	r.Header.Set("X-Alpha-User", `{"role":"admin"}`)
	r.Header.Set("Forwarded", "host=evil")
	r.Header.Set("If-None-Match", "test-etag")
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, r)
	requireStatus(t, response, 304)
	if called != 1 || response.Header().Get("ETag") != "test-etag" {
		t.Fatal("request was not proxied")
	}
	for _, path := range []string{"/api/setup", "/api/login", "/api/users", "/api/members", "/api/worker/authorization"} {
		requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+node.ID+path, nil), 404)
	}
	for _, path := range []string{"/", "/index.html", "/api/setup", "/api/users", "/api/members/register"} {
		req := httptest.NewRequest("GET", path, nil)
		authenticate(req, Node{ID: node.ID, Token: node.Token}, f.user)
		w := httptest.NewRecorder()
		node.ServeHTTP(w, req)
		requireStatus(t, w, 404)
	}
	req := httptest.NewRequest("GET", "/api/worker/info", nil)
	w := httptest.NewRecorder()
	node.ServeHTTP(w, req)
	requireStatus(t, w, 401)
	req = httptest.NewRequest("POST", "/api/jobs", nil)
	authenticate(req, Node{ID: "wrong", Token: node.Token}, f.user)
	w = httptest.NewRecorder()
	node.ServeHTTP(w, req)
	requireStatus(t, w, 409)
	req = httptest.NewRequest("POST", "/api/jobs", nil)
	viewer := f.user
	viewer.Role = "viewer"
	authenticate(req, Node{ID: node.ID, Token: node.Token}, viewer)
	w = httptest.NewRecorder()
	node.ServeHTTP(w, req)
	requireStatus(t, w, 403)
	// Existing local guards still protect the proxy from CSRF and readonly writes.
	oldCSRF := f.csrf
	f.csrf = "wrong"
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+node.ID+"/api/jobs", map[string]any{}), 403)
	f.csrf = oldCSRF
	_, err := f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "DELETE", "/api/cluster/nodes/"+node.ID, nil), 403)
	requireStatus(t, f.request(t, "GET", "/api/members", nil), 403)
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+node.ID+"/api/settings", map[string]any{}), 403)
}

func TestCentralAgentRevocation(t *testing.T) {
	f := setup(t)
	node, s := worker(t, strings.Repeat("a", 32), Inventory{}, nil)
	add(t, f, node, s, "worker")
	if _, err := f.control.agentUser(node.ID, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.Exec("UPDATE users SET enabled=0 WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.agentUser(node.ID, f.user.ID); err == nil {
		t.Fatal("disabled admin retained access")
	}
}

func TestProxyStreamFlushAndLogout(t *testing.T) {
	f := setup(t)
	stopped := make(chan struct{})
	mod := moduleFunc(func(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if _, err := f.db.RequestSession(r); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: ready\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
		return 0, nil, nil
	})
	node, s := worker(t, strings.Repeat("f", 32), Inventory{}, mod)
	add(t, f, node, s, "stream")
	controlServer := httptest.NewServer(f.server)
	defer controlServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", controlServer.URL+"/api/cluster/nodes/"+node.ID+"/api/jobs/events", nil)
	req.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: f.token})
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw := make([]byte, len("data: ready\n\n"))
	if _, err = io.ReadFull(response.Body, raw); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Logout(f.token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("logout did not terminate stream")
	}
}

func TestRegistrationRejectsRedirectsAndInvalidOwners(t *testing.T) {
	f := setup(t)
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": "10.0.0.11", "name": "redirect", "url": redirect.URL, "token": strings.Repeat("x", 32)}), 502)
	if leaked {
		t.Fatal("followed redirect with credentials")
	}
	node, s := worker(t, strings.Repeat("a", 32), Inventory{}, moduleFunc(func(http.ResponseWriter, *http.Request, platform.User) (int, any, error) {
		t.Error("invalid owner forwarded")
		return 200, nil, nil
	}))
	add(t, f, node, s, "worker")
	for _, owner := range []string{"", "not-registered"} {
		requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+node.ID+"/api/containers", map[string]string{"name": "test", "owner": owner}), 400)
	}
}
