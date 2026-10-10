package cluster

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
	"project-alpha/internal/updater"
	"project-alpha/internal/updates"
)

func TestMismatchedBusinessProtocolKeepsHealthAndUpdates(t *testing.T) {
	f := setup(t)
	id := strings.Repeat("b", 32)
	directory := t.TempDir()
	m, err := updates.New(directory, "worker", id)
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
	const githubToken = "remote-github-test-token"
	r = f.request(t, "PUT", "/api/updates/github-token", map[string]any{
		"revision": 1,
		"token":    githubToken,
	})
	requireStatus(t, r, 200)
	if strings.Contains(r.Body.String(), githubToken) || !strings.Contains(r.Body.String(), `"has_token":true`) || m.GitHubToken() != githubToken {
		t.Fatal("remote token status missing or token exposed")
	}
	requireStatus(t, f.request(t, "PUT", "/api/updates/"+id+"/settings", map[string]any{
		"revision": 1,
		"config":   updates.Config{Command: "/opt/bin/alpha-updater", Repo: "arusuki/alpha", GitHubToken: "independent-token"},
	}), 400)
	r = f.request(t, "GET", "/api/updates/"+id+"/settings", nil)
	requireStatus(t, r, 200)
	if strings.Contains(r.Body.String(), githubToken) || !strings.Contains(r.Body.String(), `"has_github_token":true`) {
		t.Fatal("remote token status not retained or token exposed")
	}
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
	waitRelease(t, m, notice.Delivery)
	health, _ := json.Marshal(m.Health())
	if !strings.Contains(string(health), "v0.8.0") {
		t.Fatal("worker did not receive release")
	}
	// Failure and recovery details use the stable management route even when
	// the business protocol is incompatible. A CLI success replaces the report.
	if err := m.Trigger("v0.8.0"); err == nil {
		t.Fatal("missing updater accepted")
	}
	r = f.request(t, "GET", "/api/updates/"+id+"/health", nil)
	requireStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), `"update_state":"failed"`) || !strings.Contains(r.Body.String(), "--data-dir") || !strings.Contains(r.Body.String(), "--tag") {
		t.Fatalf("missing remote failure report: %s", r.Body.String())
	}
	if err := updater.WriteJSON(filepath.Join(directory, "update-result.json"), (updater.ServicePlan{}).Result(nil)); err != nil {
		t.Fatal(err)
	}
	r = f.request(t, "GET", "/api/updates/"+id+"/health", nil)
	requireStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), `"update_state":"completed"`) || !strings.Contains(r.Body.String(), `"recovery":null`) || strings.Contains(r.Body.String(), `"state":"failed"`) {
		t.Fatalf("remote failure not cleared: %s", r.Body.String())
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

func TestSharedGitHubTokenRotationAndOfflineDelivery(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	f := setup(t)
	id := strings.Repeat("d", 32)
	m, err := updates.New(t.TempDir(), "worker", id)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	w := &Worker{ID: id, Token: strings.Repeat("s", 32), Updates: m}
	var offline atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			rw.WriteHeader(503)
			return
		}
		w.ServeHTTP(rw, r)
	}))
	defer server.Close()
	// A newly added node receives the already configured central token.
	requireStatus(t, f.request(t, "PUT", "/api/updates/github-token", map[string]any{"revision": 1, "token": "first-token"}), 200)
	add(t, f, w, server, "worker")
	if m.GitHubToken() != "first-token" {
		t.Fatal("new node missed shared token")
	}
	offline.Store(true)
	r := f.request(t, "PUT", "/api/updates/github-token", map[string]any{"revision": 2, "token": "rotated-token"})
	requireStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), id) || strings.Contains(r.Body.String(), "rotated-token") || m.GitHubToken() != "first-token" {
		t.Fatal("offline delivery was not reported correctly")
	}
	offline.Store(false)
	n, err := f.control.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.probe(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if m.GitHubToken() != "rotated-token" {
		t.Fatal("reconnected node missed token rotation")
	}
	// Clearing is also delivered, and stale form saves cannot undo it.
	requireStatus(t, f.request(t, "PUT", "/api/updates/github-token", map[string]any{"revision": 3, "clear": true}), 200)
	if m.GitHubToken() != "" {
		t.Fatal("node retained cleared token")
	}
	requireStatus(t, f.request(t, "PUT", "/api/updates/github-token", map[string]any{"revision": 3, "token": "stale-token"}), 409)
	if m.GitHubToken() != "" {
		t.Fatal("stale save changed node token")
	}
	// Even without a webhook, manual update first repairs a stale local copy.
	if err := m.ApplyGitHubToken("stale-copy"); err != nil {
		t.Fatal(err)
	}
	f.request(t, "POST", "/api/updates/"+id+"/update", map[string]any{})
	if m.GitHubToken() != "" {
		t.Fatal("manual update did not synchronize token first")
	}
}

func TestWebhookQueuedOfflineReachesControlAndWorker(t *testing.T) {
	f := setup(t)
	if err := f.control.Updates.SaveGitHubToken(1, "shared-webhook-token", false); err != nil {
		t.Fatal(err)
	}
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
	waitRelease(t, m, "offline-delivery")
	for _, manager := range []*updates.Manager{m, regUpdates} {
		if manager.GitHubToken() != "shared-webhook-token" {
			t.Fatal("release forwarding did not first distribute the shared token")
		}
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

func waitRelease(t *testing.T, m *updates.Manager, delivery string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := m.Health()["release"].(*updates.Release); ok && r != nil && r.Delivery == delivery {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("release not delivered", delivery)
}

func TestOfflineWorkerDoesNotBlockReleaseAcknowledgement(t *testing.T) {
	f := setup(t)
	var offline atomic.Bool
	var workers []*updates.Manager
	var offlineNode Node
	for _, id := range []string{strings.Repeat("d", 32), strings.Repeat("e", 32)} {
		m, err := updates.New(t.TempDir(), "worker", id)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		workers = append(workers, m)
		w := &Worker{ID: id, Token: strings.Repeat("s", 32), Updates: m}
		server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if id == strings.Repeat("d", 32) && offline.Load() {
				rw.WriteHeader(503)
				return
			}
			w.ServeHTTP(rw, r)
		}))
		defer server.Close()
		add(t, f, w, server, "worker")
		if id == strings.Repeat("d", 32) {
			offlineNode, err = f.control.node(id)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	offline.Store(true)
	notice := updates.Release{Delivery: "independent", Repo: "arusuki/alpha", Tag: "v0.9.0", Published: time.Now()}
	raw, _ := json.Marshal(notice)
	if _, err := f.control.releaseNotice(context.Background(), raw); err != nil {
		t.Fatal("offline worker blocked acknowledgement", err)
	}
	waitRelease(t, workers[1], notice.Delivery)
	persisted, err := updates.New(f.control.DB.Directory, "control", f.control.identity)
	if err != nil {
		t.Fatal(err)
	}
	defer persisted.Close()
	if len(persisted.PendingForNode(offlineNode.ID)) != 1 {
		t.Fatal("offline delivery was not durable")
	}
	offline.Store(false)
	// Existing node probes also retry, even without a registry connection.
	if _, err := f.control.probe(context.Background(), offlineNode); err != nil {
		t.Fatal(err)
	}
	waitRelease(t, workers[0], notice.Delivery)
	if len(f.control.Updates.PendingForNode(offlineNode.ID)) != 0 {
		t.Fatal("reconnected worker remains pending")
	}
}

func TestUpdateDeliveryIsIndependentAndTokenRotationIsOrdered(t *testing.T) {
	m, err := updates.New(t.TempDir(), "control", strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.SaveGitHubToken(1, "old-token", false); err != nil {
		t.Fatal(err)
	}
	started, releaseSlow := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(releaseSlow) })
	received := make(chan string, 4)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Token == "old-token" {
			close(started)
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
		}
		received <- body.Token
		httpapi.WriteJSON(w, 200, map[string]bool{"ok": true})
	}))
	defer slow.Close()
	// Unblock before closing the HTTP server even if an assertion fails.
	defer once.Do(func() { close(releaseSlow) })
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { httpapi.WriteJSON(w, 200, map[string]bool{"ok": true}) }))
	defer fast.Close()
	h := &Control{Updates: m, client: http.DefaultClient}
	slowNode := Node{ID: "slow", URL: slow.URL}
	slowDone := make(chan struct{})
	go func() { defer close(slowDone); h.syncUpdateNode(context.Background(), slowNode) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("slow delivery did not start")
	}
	fastDone := make(chan struct{})
	go func() { defer close(fastDone); h.syncUpdateNode(context.Background(), Node{ID: "fast", URL: fast.URL}) }()
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("slow node blocked independent delivery")
	}
	rotated := make(chan error, 1)
	go func() {
		h.updateTokenMu.Lock()
		defer h.updateTokenMu.Unlock()
		if err := m.SaveGitHubToken(2, "new-token", false); err != nil {
			rotated <- err
			return
		}
		rotated <- h.pushGitHubToken(context.Background(), slowNode, m.GitHubToken())
	}()
	select {
	case err := <-rotated:
		t.Fatal("rotation overtook in-flight token", err)
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(releaseSlow) })
	<-slowDone
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	if first, second := <-received, <-received; first != "old-token" || second != "new-token" {
		t.Fatal("stale token arrived after rotation", first, second)
	}
}
