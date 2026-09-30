package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

func TestWorkerManualReconnectCredentialsPermissionsAndShutdown(t *testing.T) {
	f := setup(t)
	w, s := worker(t, strings.Repeat("a", 32), Inventory{Containers: []Container{}}, nil)
	add(t, f, w, s, "Worker")
	path := "/api/cluster/nodes/" + w.ID + "/reconnect"
	var calls atomic.Int32
	w.Inventory = func() (Inventory, error) {
		calls.Add(1)
		return Inventory{Containers: []Container{}}, nil
	}
	csrf := f.csrf
	f.csrf = ""
	requireStatus(t, f.request(t, "POST", path, nil), 403)
	f.csrf = csrf
	if _, err := f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", path, nil), 403)
	if calls.Load() != 0 {
		t.Fatal("unauthorized reconnect reached worker")
	}
	if _, err := f.db.SQL.Exec("UPDATE users SET role='admin' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.Exec("UPDATE cluster_nodes SET token=? WHERE id=?", strings.Repeat("wrong", 10), w.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", path, nil), 502)
	if _, err := f.db.SQL.Exec("UPDATE cluster_nodes SET token=? WHERE id=?", w.Token, w.ID); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "POST", path, nil)
	requireStatus(t, r, 200)
	if calls.Load() != 1 || strings.Contains(r.Body.String(), w.Token) {
		t.Fatalf("reconnect did not check inventory safely: %s / %d", r.Body.String(), calls.Load())
	}
	w.ID = strings.Repeat("b", 32)
	requireStatus(t, f.request(t, "POST", path, nil), 409)
	var audited int
	if err := f.db.SQL.QueryRow("SELECT count(*) FROM audit WHERE action='cluster.node.reconnect'").Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("reconnect attempts not audited: %d %v", audited, err)
	}
	requireStatus(t, f.request(t, "GET", path, nil), 404)
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+strings.Repeat("c", 32)+"/reconnect", nil), 404)
	f.control.Close()
	requireStatus(t, f.request(t, "POST", path, nil), 503)
}

func TestRegistryManualReconnectInterruptsBackoffAndKeepsOtherLinks(t *testing.T) {
	f := setup(t)
	db, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	token := strings.Repeat("r", 64)
	h := registry.NewServer(db, "Abcd1234", token, nil, false)
	var available atomic.Bool
	attempts := make(chan struct{}, 16)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registry.LinkPath {
			attempts <- struct{}{}
			if !available.Load() {
				http.Error(w, "temporarily unavailable", 503)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	t.Cleanup(h.Hub.Close)
	r := f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "Gateway", "url": s.URL, "token": token})
	requireStatus(t, r, 201)
	var n Node
	if err := json.Unmarshal(r.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	path := "/api/cluster/nodes/" + n.ID
	// Three failed attempts put the link in the four-second retry delay.
	for range 3 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("registry did not automatically retry")
		}
	}
	awaitRegistryState(t, f.control, n.ID, "reconnecting")
	f.control.registryMu.Lock()
	previous := f.control.registryLinks[n.ID]
	f.control.registryMu.Unlock()
	available.Store(true)
	csrf := f.csrf
	f.csrf = "wrong"
	requireStatus(t, f.request(t, "POST", path+"/reconnect", nil), 403)
	f.csrf = csrf
	select {
	case <-previous.done:
		t.Fatal("CSRF failure restarted the link")
	default:
	}
	r = f.request(t, "POST", path+"/reconnect", nil)
	requireStatus(t, r, 202)
	select {
	case <-attempts:
	case <-time.After(2 * time.Second):
		t.Fatal("manual reconnect waited for the automatic retry delay")
	}
	awaitRegistryState(t, f.control, n.ID, "connected")
	select {
	case <-previous.done:
	default:
		t.Fatal("previous retry loop is still running")
	}
	// An unrelated registry must keep its existing connection.
	otherDB, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherDB.SQL.Close() })
	_, other := registryNodeServer(t, otherDB, token)
	r = f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "Other", "url": other.URL, "token": token})
	requireStatus(t, r, 201)
	var otherNode Node
	if err := json.Unmarshal(r.Body.Bytes(), &otherNode); err != nil {
		t.Fatal(err)
	}
	awaitRegistryState(t, f.control, otherNode.ID, "connected")
	f.control.registryMu.Lock()
	otherLink := f.control.registryLinks[otherNode.ID]
	f.control.registryMu.Unlock()
	requireStatus(t, f.request(t, "POST", path+"/reconnect", nil), 202)
	awaitRegistryState(t, f.control, n.ID, "connected")
	select {
	case <-otherLink.done:
		t.Fatal("reconnect interrupted an unrelated registry")
	default:
	}
	if _, err := f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", path+"/reconnect", nil), 403)
	if _, err := f.db.SQL.Exec("UPDATE users SET role='admin' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "DELETE", path, nil), 200)
	requireStatus(t, f.request(t, "POST", path+"/reconnect", nil), 404)
	if f.control.registryStatus(n.ID).State != "disconnected" {
		t.Fatal("removed registry was restarted")
	}
}
