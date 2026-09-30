package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

func TestRegistrationLinkRejectsChangedRegistryIdentityAndInvalidEntry(t *testing.T) {
	f := setup(t)
	store := &members.Store{Database: f.db}
	i, err := store.CreateInvitation("入口验证", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	info := Info{Protocol: registry.Protocol, ID: id, Mode: "registry", RegistrationPath: "/registry/Abcd1234/"}
	var response atomic.Value
	response.Store(info)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registry.InfoPath || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 64) || r.Header.Get("X-Alpha-Control") != f.control.identity {
			t.Error("registry discovery was not authenticated")
		}
		json.NewEncoder(w).Encode(response.Load())
	}))
	defer s.Close()
	if _, err = f.db.SQL.Exec("INSERT INTO cluster_nodes VALUES(?,?,?,?,?,?,?)", id, "Gateway", s.URL, strings.Repeat("s", 64), platform.Now(), "registry", ""); err != nil {
		t.Fatal(err)
	}
	path := "/api/cluster/nodes/" + id + "/registration-link"
	body := map[string]string{"invitation_id": i.ID}
	for _, invalid := range []string{"", "/registry/short/", "https://other.example/registry/Abcd1234/", "/registry/Abcd1234/../"} {
		info.RegistrationPath = invalid
		response.Store(info)
		requireStatus(t, f.request(t, "POST", path, body), 502)
	}
	info.RegistrationPath, info.ID = "/registry/Abcd1234/", strings.Repeat("b", 32)
	response.Store(info)
	requireStatus(t, f.request(t, "POST", path, body), 409)
}

func TestRegistryRegistrationLinks(t *testing.T) {
	f := setup(t)
	store := &members.Store{Database: f.db}
	i, err := store.CreateInvitation("分享", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var server *httptest.Server
	for _, pass := range []string{"Abcd1234", "Efgh5678"} {
		db, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.SQL.Close() })
		h := registry.NewServer(db, pass, strings.Repeat("s", 64), nil, false)
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		t.Cleanup(h.Hub.Close)
		r := f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "Public", "url": s.URL, "token": h.Hub.Token})
		requireStatus(t, r, 201)
		var node Node
		if err = json.Unmarshal(r.Body.Bytes(), &node); err != nil {
			t.Fatal(err)
		}
		path = "/api/cluster/nodes/" + node.ID + "/registration-link"
		r = f.request(t, "POST", path, map[string]string{"invitation_id": i.ID})
		requireStatus(t, r, 200)
		var result struct {
			URL string `json:"url"`
		}
		if err = json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.URL != s.URL+"/registry/"+pass+"/"+i.Code {
			t.Fatalf("incorrect registry link: %s %v", r.Body.String(), err)
		}
		overview := f.request(t, "GET", "/api/cluster/overview", nil)
		requireStatus(t, overview, 200)
		if strings.Contains(overview.Body.String(), pass) || strings.Contains(overview.Body.String(), i.Code) {
			t.Fatal("overview exposed registration credentials")
		}
		server = s
	}
	rows, err := store.Invitations()
	if err != nil || len(rows) != 1 || rows[0].Used != 0 || rows[0].Code != "" {
		t.Fatalf("sharing consumed quota or exposed code: %+v %v", rows, err)
	}
	for _, body := range []map[string]string{{}, {"invitation_id": "invalid"}, {"invitation_id": strings.Repeat("0", 32)}, {"invitation_id": i.ID, "unexpected": "field"}} {
		requireStatus(t, f.request(t, "POST", path, body), 400)
	}
	csrf := f.csrf
	f.csrf = ""
	requireStatus(t, f.request(t, "POST", path, map[string]string{"invitation_id": i.ID}), 403)
	f.csrf = csrf
	token := f.token
	f.token = ""
	requireStatus(t, f.request(t, "POST", path, map[string]string{"invitation_id": i.ID}), 401)
	f.token = token
	if _, err = f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", path, map[string]string{"invitation_id": i.ID}), 403)
	if _, err = f.db.SQL.Exec("UPDATE users SET role='admin' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.RevokeInvitation(i.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", path, map[string]string{"invitation_id": i.ID}), 400)
	server.Close()
	requireStatus(t, f.request(t, "POST", path, map[string]string{"invitation_id": i.ID}), 502)
	w, s := worker(t, strings.Repeat("b", 32), Inventory{Containers: []Container{}}, nil)
	add(t, f, w, s, "worker")
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+w.ID+"/registration-link", map[string]string{"invitation_id": i.ID}), 400)
}
