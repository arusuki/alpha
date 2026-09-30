package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMemberStatusPageAndIsolation(t *testing.T) {
	f := setup(t)
	alice, token := registerResource(t, f, "alice")
	awaitIdle(t, f, alice)
	bob, bobToken := registerResource(t, f, "bob")
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
	if _, err := f.db.SQL.Exec(`INSERT INTO member_node_resources(member_id,node_id,state,container_id,name,port,ssh_host,updated_at) VALUES(?,?,'ready',?,'alice-offline',2222,'ssh.test',0)`, alice, offline.ID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	offlineServer.Close()
	for _, path := range []string{"/status/" + alice, "/status/" + alice + "/", "/status.js", "/status.css"} {
		page := selfCall(f, "GET", path, "", nil)
		requireStatus(t, page, 200)
		if strings.HasPrefix(path, "/status/") && (!strings.Contains(page.Body.String(), "tokenForm") || strings.Contains(page.Body.String(), "authUsername")) {
			t.Fatal("status page must have its own member access form")
		}
	}
	path := "/api/status/" + alice
	requireStatus(t, selfCall(f, "GET", path, "", nil), 401)
	requireStatus(t, f.request(t, "GET", path, nil), 401)
	requireStatus(t, selfCall(f, "GET", path, bobToken, nil), 403)
	requireStatus(t, selfCall(f, "POST", path+"/containers", bobToken, map[string]string{"node_id": w.ID}), 403)
	response := selfCall(f, "GET", path, token, nil)
	requireStatus(t, response, 200)
	for _, secret := range []string{token, w.Token, "bob-private", "bob-container", "private-scan-detail", bob} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("status leaked %q", secret)
		}
	}
	var view struct {
		MemberID string             `json:"member_id"`
		Username string             `json:"username"`
		Nodes    []memberNodeStatus `json:"nodes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.MemberID != alice || view.Username != "alice" || len(view.Nodes) != 2 {
		t.Fatalf("bad member status: %+v", view)
	}
	n := view.Nodes[0]
	if !n.Online || n.Host != "compute-one" || n.ContainerCount != 2 || !n.Scanning || len(n.Containers) != 1 || n.Containers[0].Name != "alice-existing" || n.State != "unallocated" {
		t.Fatalf("bad live status: %+v", n)
	}
	n = view.Nodes[1]
	if n.Online || n.ConnectionError == "" || n.Name != "alice-offline" || n.State != "ready" {
		t.Fatalf("lost offline allocation: %+v", n)
	}
	// The page and its assets are exposed only by control.
	f.server.Control = false
	requireStatus(t, f.request(t, "GET", "/status/"+alice, nil), 404)
}

func TestStatusApplicationReturnsPersistedResult(t *testing.T) {
	f := setup(t)
	id, token := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	var fail atomic.Bool
	var calls atomic.Int32
	fail.Store(true)
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		calls.Add(1)
		if r.URL.Path != "/api/containers/members/"+id || r.Method != "PUT" || u.ID != id {
			t.Errorf("wrong worker operation: %s %s %+v", r.Method, r.URL.Path, u)
		}
		if fail.Load() {
			return 0, nil, httpapi.NewError(409, "默认镜像不可用")
		}
		return 200, map[string]any{"id": strings.Repeat("a", 64), "name": "alpha-" + id, "port": 2222, "ssh_host": "node.test"}, nil
	})
	w, s := worker(t, strings.Repeat("3", 32), Inventory{}, module)
	add(t, f, w, s, "New node")
	path := "/api/status/" + id + "/containers"
	body := map[string]string{"node_id": w.ID}
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
	requireStatus(t, selfCall(f, "POST", path, token, map[string]string{"node_id": offline.ID}), 502)
	var count int
	if err := f.db.SQL.QueryRow("SELECT count(*) FROM member_node_resources WHERE member_id=? AND node_id=?", id, offline.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("offline application was queued: %d %v", count, err)
	}
}
