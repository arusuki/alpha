package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
	"project-alpha/internal/tailscale"
)

type registryNetwork struct{ release <-chan struct{} }

func (n registryNetwork) Dispatch(http.ResponseWriter, *http.Request, platform.User) (int, any, error) {
	return 404, nil, nil
}
func (n registryNetwork) Devices(context.Context) ([]tailscale.Device, error) { return nil, nil }
func (n registryNetwork) Invites(context.Context, string) ([]tailscale.Invite, error) {
	return []tailscale.Invite{}, nil
}
func (n registryNetwork) CreateInvite(ctx context.Context, _ string) (tailscale.Invite, error) {
	select {
	case <-ctx.Done():
		return tailscale.Invite{}, ctx.Err()
	case <-n.release:
	}
	return tailscale.Invite{ID: "test-invite", URL: "https://login.tailscale.com/admin/invite/test-share"}, nil
}
func (n registryNetwork) DeleteInvite(context.Context, string) error { return nil }

func registryFixture(t *testing.T, f *fixture) (*registry.Server, *httptest.Server, *http.Client) {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), registry.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	h := registry.NewServer(db, "Abcd1234", platform.RandomHex(32), nil, false)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	t.Cleanup(h.Hub.Close)
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "registry", "name": "Public registration", "url": s.URL, "token": h.Hub.Token}), 201)
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err = h.Hub.Call(context.Background(), registry.Request{Action: "ping"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	jar, _ := cookiejar.New(nil)
	return h, s, &http.Client{Jar: jar, Timeout: 5 * time.Second}
}
func registryHTTP(t *testing.T, client *http.Client, method, address, origin, csrf string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r, _ := http.NewRequest(method, address, bytes.NewReader(raw))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}
func TestRegistryOutboundRegistrationProgressAndMultipleGateways(t *testing.T) {
	f := setup(t)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.control.Bastion.Tailscale = registryNetwork{release}
	f.control.Bastion.KeyEditor = func(context.Context, string, string) error { return nil }
	if _, err := f.db.SQL.Exec(`INSERT INTO bastion_tailscale VALUES('device','network',1,'100.64.0.2',22,8765,'http://10.0.0.1:8765')`); err != nil {
		t.Fatal(err)
	}
	store := &members.Store{Database: f.db}
	i, err := store.CreateInvitation("registry", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	nodeID := strings.Repeat("1", 32)
	w, ws := worker(t, nodeID, Inventory{}, moduleFunc(func(http.ResponseWriter, *http.Request, platform.User) (int, any, error) {
		return 200, map[string]any{"id": strings.Repeat("c", 64), "name": "workspace", "port": 2222, "ssh_host": "worker.internal"}, nil
	}))
	add(t, f, w, ws, "Compute")
	h, s, client := registryFixture(t, f)
	_, second, secondClient := registryFixture(t, f) // Same control, two independent public listeners.
	base := "/registry/Abcd1234/" + i.Code
	for _, path := range []string{"/", "/app.js", "/api/session", "/registry/Wrong123/" + i.Code, "/registry/Abcd1234/" + strings.Repeat("0", 48), base + "/api/session"} {
		resp, err := client.Get(s.URL + path)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("uninvited route responded: %s %d", path, resp.StatusCode)
		}
	}
	if status, _ := registryHTTP(t, secondClient, "GET", second.URL+base, "", "", nil); status != 200 {
		t.Fatal(status)
	}
	if status, raw := registryHTTP(t, client, "GET", s.URL+base, "", "", nil); status != 200 || !bytes.Contains(raw, []byte("开始注册")) {
		t.Fatalf("entry %d %s", status, raw)
	}
	_, raw := registryHTTP(t, client, "GET", s.URL+base+"/api/session", "", "", nil)
	var initial struct {
		CSRF   string         `json:"csrf"`
		Schema members.Schema `json:"schema"`
	}
	if err = json.Unmarshal(raw, &initial); err != nil || initial.CSRF == "" {
		t.Fatalf("session %s %v", raw, err)
	}
	if strings.Contains(string(raw), "resource_token") {
		t.Fatal("resource token was returned before registration")
	}
	body := map[string]any{"username": "alice", "ssh_public_key": resourceTestKey, "schema_revision": 1, "profile": map[string]string{}}
	for _, origin := range []string{"https://evil.test", ""} {
		b, _ := json.Marshal(body)
		r, _ := http.NewRequest("POST", s.URL+base+"/api/register", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", initial.CSRF)
		resp, err := client.Do(r)
		if err == nil {
			resp.Body.Close()
			t.Fatal("cross-site registration accepted")
		}
	}
	status, registered := registryHTTP(t, client, "POST", s.URL+base+"/api/register", s.URL, initial.CSRF, body)
	if status != 200 {
		t.Fatalf("register %d %s", status, registered)
	}
	var registrationResult struct {
		Token string `json:"resource_token"`
	}
	if err = json.Unmarshal(registered, &registrationResult); err != nil || len(registrationResult.Token) != 64 {
		t.Fatalf("registration did not return the member token: %s %v", registered, err)
	}
	_, restored := registryHTTP(t, client, "GET", s.URL+base+"/api/session", "", "", nil)
	var restoredResult struct {
		Token string `json:"resource_token"`
	}
	if err = json.Unmarshal(restored, &restoredResult); err != nil || restoredResult.Token != registrationResult.Token {
		t.Fatalf("registry refresh lost member token: %s %v", restored, err)
	}
	// Emulate a committed registration whose acknowledgment was lost on the gateway.
	if _, err = h.DB.SQL.Exec("UPDATE registry_sessions SET registered=0"); err != nil {
		t.Fatal(err)
	}
	if status, raw := registryHTTP(t, client, "POST", s.URL+base+"/api/register", s.URL, initial.CSRF, map[string]string{}); status != 200 {
		t.Fatalf("recovery %d %s", status, raw)
	}
	var used, count int
	f.db.SQL.QueryRow("SELECT used FROM member_invitations WHERE id=?", i.ID).Scan(&used)
	f.db.SQL.QueryRow("SELECT count(*) FROM members").Scan(&count)
	if used != 1 || count != 1 {
		t.Fatalf("duplicate registration: used=%d members=%d", used, count)
	}
	// Removing the invitation must still allow recovery and resource access for
	// this committed registration, while denying new registration sessions.
	if err = store.DeleteInvitation(i.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.DB.SQL.Exec("UPDATE registry_sessions SET registered=0"); err != nil {
		t.Fatal(err)
	}
	if status, raw := registryHTTP(t, client, "POST", s.URL+base+"/api/register", s.URL, initial.CSRF, map[string]string{}); status != 200 {
		t.Fatalf("recovery after invitation deletion %d %s", status, raw)
	}
	if err = f.db.SQL.QueryRow("SELECT count(*) FROM members").Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery duplicated members: %d %v", count, err)
	}
	freshClient := &http.Client{Timeout: 5 * time.Second}
	if response, err := freshClient.Get(s.URL + base); err == nil {
		response.Body.Close()
		t.Fatal("deleted invitation allowed a new registry session")
	}
	if status, _ := registryHTTP(t, client, "GET", s.URL+base, "", "", nil); status != 200 {
		t.Fatal("refresh lost committed session after invitation deletion")
	}
	// Another registry does not get a quota exception from this browser's session.
	_, secondRaw := registryHTTP(t, secondClient, "GET", second.URL+base+"/api/session", "", "", nil)
	var secondSession struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(secondRaw, &secondSession)
	body["username"] = "bob"
	if status, _ := registryHTTP(t, secondClient, "POST", second.URL+base+"/api/register", second.URL, secondSession.CSRF, body); status != 400 {
		t.Fatalf("quota bypass: %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+base+"/api/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	scanner := bufio.NewScanner(resp.Body)
	seenPending := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if strings.Contains(line, "resource_token") || strings.Contains(line, w.Token) {
			t.Fatal("progress leaked credentials")
		}
		var view memberResourceView
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &view); err != nil {
			t.Fatal(err)
		}
		if !seenPending {
			if view.Access.InviteURL != "" {
				t.Fatal("premature sharing result")
			}
			seenPending = true
			close(release)
		}
		if view.Access.InviteState == "invited" && view.Access.KeyState == "ready" && len(view.Nodes) == 1 && view.Nodes[0].State == "ready" {
			if view.Control.InternalIP != "10.0.0.1" || view.Control.StatusURL != "http://100.64.0.2:8765/status/alice" || view.Nodes[0].InternalIP != "10.0.0.11" {
				t.Fatalf("registration guidance lost configured IPs: %+v", view)
			}
			if view.Access.InviteURL != "https://login.tailscale.com/admin/invite/test-share" {
				t.Fatal(view.Access)
			}
			gateways, err := f.control.nodes("registry")
			if err != nil || len(gateways) != 2 || gateways[0].Token == gateways[1].Token {
				t.Fatalf("independent registry credentials: %v", err)
			}
			requireStatus(t, f.request(t, "POST", "/api/members/"+view.MemberID+"/containers", map[string]string{"node_id": gateways[0].ID}), 400)
			_, result, err := f.control.memberStatus(context.Background(), view.MemberID)
			if err != nil || len(result.(map[string]any)["nodes"].([]memberNodeStatus)) != 1 {
				t.Fatalf("registry included in member status: %v", err)
			}
			return
		}
	}
	t.Fatalf("progress did not reach ready: %v", scanner.Err())
}
