package cluster

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

func registryNodeServer(t *testing.T, db *platform.Database, token string) (*registry.Server, *httptest.Server) {
	t.Helper()
	h := registry.NewServer(db, "Abcd1234", token, nil, false)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	t.Cleanup(h.Hub.Close)
	return h, s
}

func awaitRegistryState(t *testing.T, h *Control, id, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.registryStatus(id).State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("registry state: got %+v, want %s", h.registryStatus(id), state)
}

func TestRegistryNodeLifecycleCredentialsAndRestart(t *testing.T) {
	f := setup(t)
	db, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	token := strings.Repeat("r", 64)
	h, s := registryNodeServer(t, db, token)
	body := map[string]string{"kind": "registry", "name": "Public", "url": s.URL, "token": token}
	// Discovery must validate credentials without claiming an unused gateway.
	n := Node{Kind: "registry", URL: s.URL, Token: token}
	info, err := f.control.probe(context.Background(), n)
	if err != nil || info.Mode != "registry" {
		t.Fatalf("probe: %+v %v", info, err)
	}
	var bound int
	if err := db.SQL.QueryRow("SELECT count(*) FROM registry_control").Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("probe changed binding: %d %v", bound, err)
	}
	body["token"] = strings.Repeat("wrong", 10)
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 502)
	body["token"] = token
	body["kind"] = "worker"
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 502)
	body["kind"] = "registry"
	r := f.request(t, "POST", "/api/cluster/nodes", body)
	requireStatus(t, r, 201)
	var saved Node
	if err := json.Unmarshal(r.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID != info.ID || saved.Kind != "registry" || strings.Contains(r.Body.String(), token) {
		t.Fatalf("saved registry: %s", r.Body.String())
	}
	path := "/api/cluster/nodes/" + saved.ID
	awaitRegistryState(t, f.control, saved.ID, "connected")
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 409)
	for _, suffix := range []string{"/api/state", "/api/agent/sessions", "/api/containers"} {
		requireStatus(t, f.request(t, "GET", path+suffix, nil), 404)
	}
	var overview struct {
		Nodes   []NodeStatus `json:"nodes"`
		Online  int          `json:"online"`
		Count   int          `json:"container_count"`
		Partial bool         `json:"partial"`
	}
	r = f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, r, 200)
	if err := json.Unmarshal(r.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Nodes) != 1 || overview.Online != 1 || overview.Count != 0 || overview.Partial || overview.Nodes[0].Inventory != nil || overview.Nodes[0].Connection.LastSeen == 0 {
		t.Fatalf("registry overview: %s", r.Body.String())
	}
	if strings.Contains(r.Body.String(), token) {
		t.Fatal("overview leaked token")
	}
	body["token"], body["name"] = "", "Renamed"
	requireStatus(t, f.request(t, "PUT", path, body), 200)
	stored, err := f.control.node(saved.ID)
	if err != nil || stored.Token != token || stored.Name != "Renamed" {
		t.Fatalf("preserve token: %+v %v", stored, err)
	}
	body["kind"] = "worker"
	requireStatus(t, f.request(t, "PUT", path, body), 400)
	body["kind"] = "registry"
	// Replacing the remote endpoint cannot substitute another registry identity.
	otherDB, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherDB.SQL.Close() })
	_, other := registryNodeServer(t, otherDB, token)
	body["url"] = other.URL
	requireStatus(t, f.request(t, "PUT", path, body), 409)
	// Rotate the remote secret and address, then apply its new token in control.
	h.Hub.Close()
	s.Close()
	awaitRegistryState(t, f.control, saved.ID, "reconnecting")
	newToken := strings.Repeat("n", 64)
	h, s = registryNodeServer(t, db, newToken)
	body["url"] = s.URL
	requireStatus(t, f.request(t, "PUT", path, body), 502)
	body["token"] = newToken
	requireStatus(t, f.request(t, "PUT", path, body), 200)
	awaitRegistryState(t, f.control, saved.ID, "connected")
	// Another control cannot register this already-bound public gateway.
	otherControl := setup(t)
	requireStatus(t, otherControl.request(t, "POST", "/api/cluster/nodes", body), 409)
	f.control.Close()
	f.control, err = NewControl(f.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.control.Close)
	f.server.Module = f.control
	awaitRegistryState(t, f.control, saved.ID, "connected")
	// Read-only accounts see status but cannot change credentials or disconnect.
	if _, err = f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "GET", "/api/cluster/overview", nil), 200)
	for _, method := range []string{"PUT", "DELETE"} {
		requireStatus(t, f.request(t, method, path, body), 403)
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", body), 403)
	if _, err = f.db.SQL.Exec("UPDATE users SET role='admin' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "DELETE", path, nil), 200)
	if _, err = h.Hub.Call(context.Background(), registry.Request{Action: "ping"}); err == nil {
		t.Fatal("removed registry still connected")
	}
	if nodes, err := f.control.nodes(""); err != nil || len(nodes) != 0 {
		t.Fatalf("removed node persisted: %v %v", nodes, err)
	}
	var binding string
	if err := db.SQL.QueryRow("SELECT control_id FROM registry_control").Scan(&binding); err != nil || binding != f.control.identity {
		t.Fatal("removal changed registry binding")
	}
}

func TestNodeKindAndRegistryAddressValidation(t *testing.T) {
	f := setup(t)
	for _, kind := range []string{"", "control", "unknown"} {
		requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": kind, "name": "invalid", "url": "https://registry.example.com", "token": strings.Repeat("s", 32)}), 400)
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "invalid", "url": "http://registry.example.com", "token": strings.Repeat("s", 32)}), 400)
}
