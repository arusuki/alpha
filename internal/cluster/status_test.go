package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMemberStatusPageAndIsolation(t *testing.T) {
	f := setup(t)
	alice, _ := registerResource(t, f, "alice")
	token := statusLogin(t, f, "alice", "Member-password-123")
	awaitIdle(t, f, alice)
	bob, _ := registerResource(t, f, "bob")
	bobToken := statusLogin(t, f, "bob", "Member-password-123")
	awaitIdle(t, f, bob)
	w, s := worker(t, strings.Repeat("1", 32), Inventory{
		Host: "compute-one", Active: map[string]string{"secret": "private-scan-detail"},
		Containers: []Container{
			{ID: "alice-container", Name: "alice-existing", Owner: "alice", Managed: true},
			{ID: "bob-container", Name: "bob-private", Owner: "bob", Managed: true},
		},
	}, nil)
	add(t, f, w, s, "Node one")
	offline, offlineServer := worker(t, strings.Repeat("2", 32), Inventory{}, nil)
	add(t, f, offline, offlineServer, "Offline node")
	if _, err := f.db.SQL.Exec(`INSERT INTO member_node_resources(member_id,node_id,state,container_id,name,port,updated_at) VALUES(?,?,'ready',?,'alice-offline',2222,0)`, alice, offline.ID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	offlineServer.Close()
	for _, path := range []string{"/status/alice", "/status/alice/", "/status.js", "/status.css"} {
		page := selfCall(f, "GET", path, "", nil)
		requireStatus(t, page, 200)
		if strings.HasPrefix(path, "/status/") && (!strings.Contains(page.Body.String(), "loginForm") || strings.Contains(page.Body.String(), "authUsername")) {
			t.Fatal("status page must have its own member access form")
		}
	}
	path := "/api/status/alice"
	requireStatus(t, selfCall(f, "GET", path, "", nil), 401)
	requireStatus(t, f.request(t, "GET", path, nil), 401)
	requireStatus(t, selfCall(f, "GET", path, bobToken, nil), 403)
	requireStatus(t, selfCall(f, "POST", path+"/containers", bobToken, map[string]string{"node_id": w.ID, "mode": "create"}), 403)
	requireStatus(t, selfCall(f, "GET", "/api/status/bob", token, nil), 403)
	requireStatus(t, selfCall(f, "GET", "/api/status/bob", bobToken, nil), 200)
	requireStatus(t, selfCall(f, "GET", "/api/status/missing", token, nil), 403)
	for _, name := range []string{"Alice", "al", "alice.", strings.Repeat("a", 33), "0123456789abcdef0123456789abcdef"} {
		requireStatus(t, selfCall(f, "GET", "/api/status/"+name, token, nil), 404)
		requireStatus(t, selfCall(f, "POST", "/api/status/"+name+"/containers", token, map[string]string{"node_id": w.ID, "mode": "create"}), 404)
	}
	response := selfCall(f, "GET", path, token, nil)
	requireStatus(t, response, 200)
	for _, secret := range []string{token, w.Token, "bob-private", "bob-container", "private-scan-detail", bob} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("status leaked %q", secret)
		}
	}
	var view struct {
		MemberID string              `json:"member_id"`
		Username string              `json:"username"`
		Control  memberControlAccess `json:"control"`
		Nodes    []memberNodeStatus  `json:"nodes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.MemberID != alice || view.Username != "alice" || len(view.Nodes) != 2 {
		t.Fatalf("bad member status: %+v", view)
	}
	if view.Control.StatusURL != "" || view.Nodes[0].InternalIP != "10.0.0.11" {
		t.Fatalf("status did not use configured addresses: %+v", view)
	}
	n := view.Nodes[0]
	if !n.Online || n.Host != "compute-one" || n.ContainerCount != 2 || !n.Scanning || len(n.Containers) != 1 || n.Containers[0].Name != "alice-existing" || n.State != "unallocated" {
		t.Fatalf("bad live status: %+v", n)
	}
	n = view.Nodes[1]
	if n.Online || n.ConnectionError == "" || n.Name != "alice-offline" || n.State != "ready" {
		t.Fatalf("lost offline allocation: %+v", n)
	}
	// Workers expose APIs only.
	request, err := http.NewRequest("GET", s.URL+"/status/alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+w.Token)
	request.Header.Set("X-Alpha-Node", w.ID)
	request.Header.Set("X-Alpha-User", httpapi.JSONText(platform.User{ID: alice, Username: "alice", Role: "viewer"}))
	workerResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer workerResponse.Body.Close()
	if workerResponse.StatusCode != 404 {
		t.Fatalf("worker served member HTML: %d", workerResponse.StatusCode)
	}
}

func TestStatusUsernameCharactersAndLength(t *testing.T) {
	f := setup(t)
	for _, name := range []string{"a_b-c", strings.Repeat("a", 32)} {
		id, _ := registerResource(t, f, name)
		token := statusLogin(t, f, name, "Member-password-123")
		awaitIdle(t, f, id)
		requireStatus(t, selfCall(f, "GET", "/status/"+name, "", nil), 200)
		response := selfCall(f, "GET", "/api/status/"+name, token, nil)
		requireStatus(t, response, 200)
		var view struct {
			MemberID string `json:"member_id"`
			Username string `json:"username"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.MemberID != id || view.Username != name {
			t.Fatalf("wrong member for username %q: %+v", name, view)
		}
	}
}

func TestMemberGuidanceUsesCurrentConfiguredIPs(t *testing.T) {
	f := setup(t)
	id, _ := registerResource(t, f, "alice")
	token := statusLogin(t, f, "alice", "Member-password-123")
	awaitIdle(t, f, id)
	if _, err := f.db.SQL.Exec("INSERT INTO bastion_tailscale VALUES('share','Share',1,'100.64.0.3',2222,9765,'http://100.100.0.2:8765')"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.Exec("UPDATE member_access SET tailscale_id='share' WHERE member_id=?", id); err != nil {
		t.Fatal(err)
	}
	w, s := worker(t, strings.Repeat("7", 32), Inventory{}, nil)
	body := map[string]string{"kind": "worker", "name": "Compute", "url": s.URL, "token": w.Token}
	for _, ip := range []string{"", "127.0.0.1", "http://10.0.0.11", "10.0.0.11:22"} {
		body["internal_ip"] = ip
		requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 400)
	}
	body["internal_ip"] = "10.0.0.11"
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 201)
	body["token"] = ""
	body["internal_ip"] = ""
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+w.ID, body), 400)
	body["internal_ip"] = "fd00::11"
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+w.ID, body), 200)
	response := selfCall(f, "GET", "/api/status/alice", token, nil)
	requireStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "http://100.64.0.3:9765/status/alice") || !strings.Contains(response.Body.String(), "fd00::11") || strings.Contains(response.Body.String(), "10.0.0.11") || strings.Contains(response.Body.String(), s.URL) {
		t.Fatalf("guidance retained an old or management address: %s", response.Body.String())
	}
}

func TestStatusApplicationReturnsPersistedResult(t *testing.T) {
	f := setup(t)
	id, _ := registerResource(t, f, "alice")
	token := statusLogin(t, f, "alice", "Member-password-123")
	awaitIdle(t, f, id)
	var fail atomic.Bool
	var calls atomic.Int32
	fail.Store(true)
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		calls.Add(1)
		if r.URL.Path != "/api/containers/members/"+id || r.Method != "PUT" || u.ID != id {
			t.Errorf("wrong worker operation: %s %s %+v", r.Method, r.URL.Path, u)
		}
		var input struct {
			Password    string `json:"password"`
			Mode        string `json:"mode"`
			ContainerID string `json:"container_id"`
			Username    string `json:"username"`
			SSHKey      string `json:"ssh_public_key"`
		}
		if err := httpapi.DecodeBody(nil, r, &input); err != nil || input.Password != "Member-password-123" {
			t.Errorf("worker password not forwarded: %v", err)
		}
		if fail.Load() {
			return 0, nil, httpapi.NewError(409, "默认镜像不可用")
		}
		return 200, map[string]any{"id": strings.Repeat("a", 64), "name": "alpha-" + id, "port": 2222, "ssh_host": "node.test"}, nil
	})
	w, s := worker(t, strings.Repeat("3", 32), Inventory{}, module)
	add(t, f, w, s, "New node")
	path := "/api/status/alice/containers"
	body := map[string]string{"node_id": w.ID, "mode": "create"}
	response := selfCall(f, "POST", path, token, body)
	requireStatus(t, response, 409)
	if !strings.Contains(response.Body.String(), "默认镜像不可用") || calls.Load() != 1 {
		t.Fatal("creation failure must be returned in the application response")
	}
	var state, message string
	if err := f.db.SQL.QueryRow("SELECT state,error FROM member_node_resources WHERE member_id=? AND node_id=?", id, w.ID).Scan(&state, &message); err != nil || state != "failed" || message != "默认镜像不可用" {
		t.Fatalf("failure was not persisted: %s %s %v", state, message, err)
	}
	awaitIdle(t, f, id)
	if calls.Load() != 1 {
		t.Fatal("failed application retried without user action")
	}
	fail.Store(false)
	response = selfCall(f, "POST", path, token, body)
	requireStatus(t, response, 200)
	var resources memberResourceView
	if err := json.Unmarshal(response.Body.Bytes(), &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.Nodes) != 1 || resources.Nodes[0].State != "ready" || resources.Nodes[0].Name != "alpha-"+id || calls.Load() != 2 {
		t.Fatalf("application did not return completed creation: %+v", resources)
	}
	awaitIdle(t, f, id)
	s.Close()
	// Existing allocations remain idempotent, including while the node is offline.
	requireStatus(t, selfCall(f, "POST", path, token, body), 200)
	if calls.Load() != 2 {
		t.Fatal("duplicate create")
	}
	offline, server := worker(t, strings.Repeat("4", 32), Inventory{}, module)
	add(t, f, offline, server, "Offline new node")
	server.Close()
	requireStatus(t, selfCall(f, "POST", path, token, map[string]string{"node_id": offline.ID, "mode": "create"}), 502)
	var count int
	if err := f.db.SQL.QueryRow("SELECT count(*) FROM member_node_resources WHERE member_id=? AND node_id=?", id, offline.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("offline application was queued: %d %v", count, err)
	}
}

func TestShareStatusEntranceHostAndOriginAreValidated(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	token := statusLogin(t, f, "alice", "Member-password-123")
	awaitIdle(t, f, id)
	if _, err := f.db.SQL.Exec("INSERT INTO bastion_tailscale VALUES('share','Share',1,'100.64.0.2',22,9765,'http://10.0.0.1:8765')"); err != nil {
		t.Fatal(err)
	}
	request := func(host, method, path, origin string) int {
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+token)
		if strings.HasPrefix(path, "/api/members/") {
			r.Header.Set("Authorization", "Bearer "+resourceToken)
		}
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		f.server.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("10.0.0.1:8765", "GET", "/api/status/alice", ""); got != 403 {
		t.Fatal("proxy upstream automatically allowed as a browser entrance", got)
	}
	if got := request("100.64.0.2:9765", "GET", "/api/status/alice", ""); got != 200 {
		t.Fatal(got)
	}
	if got := request("100.64.0.2:9766", "GET", "/api/status/alice", ""); got != 403 {
		t.Fatal("unconfigured port accepted", got)
	}
	if got := request("100.64.0.2:9765", "POST", "/api/members/me/retry", "http://evil.example"); got != 403 {
		t.Fatal("cross origin accepted", got)
	}
	if got := request("100.64.0.2:9765", "POST", "/api/members/me/retry", "http://100.64.0.2:9765"); got != 202 {
		t.Fatal(got)
	}
}

func statusLogin(t *testing.T, f *fixture, username, password string) string {
	t.Helper()
	response := selfCall(f, "POST", "/api/status/"+username+"/login", "", map[string]string{"password": password})
	requireStatus(t, response, 200)
	var result struct {
		Token string `json:"session_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Token) != 64 {
		t.Fatalf("login: %s %v", response.Body.String(), err)
	}
	return result.Token
}

func TestStatusPasswordChangeAndSessionBoundary(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	path := "/api/status/alice"
	requireStatus(t, selfCall(f, "GET", path, resourceToken, nil), 401)
	requireStatus(t, selfCall(f, "POST", path+"/login", "", map[string]string{"password": "wrong-password"}), 401)
	token := statusLogin(t, f, "alice", "Member-password-123")
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", token, nil), 401)
	requireStatus(t, selfCall(f, "POST", path+"/password", token, map[string]string{"current_password": "wrong-password", "password": "Changed-password-123"}), 403)
	requireStatus(t, selfCall(f, "GET", path, token, nil), 200)
	requireStatus(t, selfCall(f, "POST", path+"/password", token, map[string]string{"current_password": "Member-password-123", "password": "Changed-password-123"}), 200)
	requireStatus(t, selfCall(f, "GET", path, token, nil), 401)
	requireStatus(t, selfCall(f, "POST", path+"/login", "", map[string]string{"password": "Member-password-123"}), 401)
	token = statusLogin(t, f, "alice", "Changed-password-123")
	workerNode, workerServer := worker(t, strings.Repeat("9", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		if r.URL.Path == "/api/worker/scheduled-analysis" {
			return 200, map[string]any{"jobs": []any{}}, nil
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["password"] != "Changed-password-123" {
			t.Errorf("new container did not receive updated password: %v", err)
		}
		return 200, map[string]any{"id": strings.Repeat("e", 64), "name": "alpha-" + id, "port": 2222}, nil
	}))
	add(t, f, workerNode, workerServer, "After password change")
	requireStatus(t, selfCall(f, "POST", path+"/containers", token, map[string]string{"node_id": workerNode.ID, "mode": "create"}), 200)
	awaitIdle(t, f, id)
	requireStatus(t, selfCall(f, "POST", path+"/logout", token, nil), 200)
	requireStatus(t, selfCall(f, "GET", path, token, nil), 401)
	for range 20 {
		selfCall(f, "POST", path+"/login", "", map[string]string{"password": "wrong-password"})
	}
	requireStatus(t, selfCall(f, "POST", path+"/login", "", map[string]string{"password": "wrong-password"}), 429)
}
