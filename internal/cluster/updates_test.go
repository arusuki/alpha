package cluster

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
	"project-alpha/internal/updates"
)

func TestMismatchedBusinessProtocolKeepsHealthAndUpdates(t *testing.T) {
	f := setup(t)
	id := strings.Repeat("b", 32)
	m, err := updates.New(t.TempDir(), "worker", id)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{ID: id, Token: strings.Repeat("s", 32), Updates: m}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/info" {
			httpapi.WriteJSON(rw, 200, Info{Protocol: Protocol + 99, ManagementProtocol: updates.Protocol, ID: id, Mode: "worker", Version: "v0.8.0"})
			return
		}
		w.ServeHTTP(rw, r)
	}))
	defer server.Close()
	add(t, f, w, server, "different-version")
	r := f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, r, 200)
	var overview struct {
		Nodes   []NodeStatus `json:"nodes"`
		Online  int          `json:"online"`
		Partial bool         `json:"partial"`
	}
	json.Unmarshal(r.Body.Bytes(), &overview)
	if overview.Online != 1 || !overview.Nodes[0].Online || overview.Nodes[0].Compatible || !overview.Partial {
		t.Fatalf("health tied to business protocol: %s", r.Body.String())
	}
	requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+id+"/api/state", nil), 409)
	requireStatus(t, f.request(t, "GET", "/api/updates/"+id+"/settings", nil), 200)
	req := httptest.NewRequest("GET", "http://127.0.0.1"+updates.Path+"/settings", nil)
	authenticate(req, Node{ID: id, Token: w.Token}, platform.User{ID: f.user.ID, Username: "viewer", Role: "viewer"})
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	requireStatus(t, out, 403)
	notice := updates.Release{Delivery: "delivery-1", Repo: "arusuki/alpha", Tag: "v0.8.0", Published: time.Now()}
	body, _ := json.Marshal(notice)
	for range 2 {
		if _, err = f.control.RegistryDispatch(context.Background(), registry.Request{Action: "release.v1", Protocol: 999, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	health, _ := json.Marshal(m.Health())
	if !strings.Contains(string(health), "v0.8.0") {
		t.Fatal("worker did not receive release")
	}
}
func TestUpdateEndpointsRequireAdminAndCSRF(t *testing.T) {
	f := setup(t)
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/updates/control/update", strings.NewReader(`{}`))
	r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: f.token})
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	requireStatus(t, w, 403)
	_, _, err := f.control.Dispatch(w, httptest.NewRequest("GET", "/api/updates/control/settings", nil), platform.User{Role: "viewer"})
	if err == nil {
		t.Fatal("viewer could read settings")
	}
}

func TestWebhookQueuedOfflineReachesControlAndWorker(t *testing.T) {
	f := setup(t)
	id := strings.Repeat("c", 32)
	m, err := updates.New(t.TempDir(), "worker", id)
	if err != nil {
		t.Fatal(err)
	}
	worker := &Worker{ID: id, Token: strings.Repeat("t", 32), Updates: m}
	workerHTTP := httptest.NewServer(worker)
	defer workerHTTP.Close()
	add(t, f, worker, workerHTTP, "worker")
	db, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	regID, _ := db.CheckMode("registry")
	regUpdates, err := updates.New(db.Directory, "registry", regID)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("h", 32)
	if err = regUpdates.Save(1, updates.Config{Command: "/opt/bin/alpha-updater", Repo: "arusuki/alpha", WebhookSecret: secret}); err != nil {
		t.Fatal(err)
	}
	frontend := registry.NewServer(db, "Abcd1234", strings.Repeat("r", 32), nil, false)
	frontend.Hub.Updates = regUpdates
	server := httptest.NewServer(frontend)
	defer server.Close()
	defer frontend.Hub.Close()
	body := `{"action":"published","repository":{"full_name":"arusuki/alpha"},"release":{"tag_name":"v0.3.2","published_at":"2026-10-06T00:00:00Z"}}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r, _ := http.NewRequest("POST", server.URL+updates.WebhookPath, strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", "release")
	r.Header.Set("X-GitHub-Delivery", "offline-delivery")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 202 || regUpdates.Pending() == nil {
		t.Fatal("offline delivery not saved")
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "registry", "url": server.URL, "token": frontend.Hub.Token}), 201)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && regUpdates.Pending() != nil {
		time.Sleep(10 * time.Millisecond)
	}
	if regUpdates.Pending() != nil {
		t.Fatal("queued notification not acknowledged")
	}
	for _, manager := range []*updates.Manager{m, f.control.Updates} {
		raw, _ := json.Marshal(manager.Health())
		if !strings.Contains(string(raw), "offline-delivery") {
			t.Fatalf("notice missing: %s", raw)
		}
	}
	requireStatus(t, f.request(t, "GET", "/api/updates/"+regID+"/settings", nil), 200)
	request := httptest.NewRequest("GET", "http://127.0.0.1"+updates.Path+"/settings", nil)
	request.Header.Set("Authorization", "Bearer "+frontend.Hub.Token)
	request.Header.Set("X-Alpha-Node", regID)
	request.Header.Set("X-Alpha-Control", strings.Repeat("f", 32))
	out := httptest.NewRecorder()
	frontend.ServeHTTP(out, request)
	requireStatus(t, out, 401)
}
