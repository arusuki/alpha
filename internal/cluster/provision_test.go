package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

const resourceTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

func selfCall(f *fixture, method, path, token string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}
func registerResource(t *testing.T, f *fixture, name string) (string, string) {
	t.Helper()
	store := &members.Store{Database: f.db}
	invite, e := store.CreateInvitation("resources", 1, "admin")
	if e != nil {
		t.Fatal(e)
	}
	w := selfCall(f, "POST", "/api/members/register", "", map[string]any{"username": name, "ssh_public_key": resourceTestKey, "password": "Member-password-123", "invitation_code": invite.Code, "schema_revision": 1, "profile": map[string]string{}})
	requireStatus(t, w, 201)
	var v struct {
		ID    string `json:"id"`
		Token string `json:"resource_token"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	if len(v.Token) != 64 {
		t.Fatal("no member token")
	}
	return v.ID, v.Token
}
func awaitResource(t *testing.T, f *fixture, id, node, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var s string
		e := f.db.SQL.QueryRow("SELECT state FROM member_node_resources WHERE member_id=? AND node_id=?", id, node).Scan(&s)
		if e == nil && s == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("member %s node %s never became %s", id, node, want)
}
func awaitIdle(t *testing.T, f *fixture, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending int
		e := f.db.SQL.QueryRow("SELECT pending FROM member_work WHERE member_id=?", id).Scan(&pending)
		if e == nil && pending == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("member queue never idle")
}
func TestMemberSelfServiceRetryIsolationAndUnassign(t *testing.T) {
	f := setup(t)
	var mu sync.Mutex
	fail := true
	deleteFail := false
	created := map[string]int{}
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/api/containers/members/")
		if !identifier.MatchString(id) {
			return 404, nil, nil
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["ssh_public_key"] != resourceTestKey || u.ID != id {
			t.Errorf("wrong user/key %v %+v", req, u)
		}
		if r.Method == "DELETE" {
			if deleteFail {
				return 0, nil, httpapi.NewError(503, "offline")
			}
			return 200, map[string]bool{"ok": true}, nil
		}
		if fail {
			return 0, nil, httpapi.NewError(409, "node unavailable")
		}
		if created[id] == 0 {
			created[id]++
		}
		return 200, map[string]any{"id": strings.Repeat("a", 64), "name": "alpha-" + id, "port": 2222, "ssh_host": "node.test"}, nil
	})
	nodeID := strings.Repeat("1", 32)
	w, s := worker(t, nodeID, Inventory{}, module)
	add(t, f, w, s, "Node")
	alice, token := registerResource(t, f, "alice")
	awaitResource(t, f, alice, nodeID, "failed")
	awaitIdle(t, f, alice)
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", "invalid", nil), 401)
	response := selfCall(f, "GET", "/api/members/me/resources", token, nil)
	requireStatus(t, response, 200)
	if strings.Contains(response.Body.String(), w.Token) || strings.Contains(response.Body.String(), token) {
		t.Fatal("secret leak")
	}
	requireStatus(t, selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": nodeID, "username": "someone_else"}), 400)
	mu.Lock()
	fail = false
	mu.Unlock()
	requireStatus(t, selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": nodeID}), 200)
	awaitResource(t, f, alice, nodeID, "ready")
	awaitIdle(t, f, alice)
	var requests sync.WaitGroup
	for range 12 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			response := selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": nodeID})
			if response.Code != 200 && response.Code != 409 {
				t.Errorf("retry status %d: %s", response.Code, response.Body.String())
			}
		}()
	}
	requests.Wait()
	awaitIdle(t, f, alice)
	mu.Lock()
	if created[alice] != 1 {
		t.Fatal("duplicate container")
	}
	mu.Unlock()
	bob, bobToken := registerResource(t, f, "bob")
	awaitResource(t, f, bob, nodeID, "ready")
	awaitIdle(t, f, bob)
	response = selfCall(f, "GET", "/api/members/me/resources", bobToken, nil)
	requireStatus(t, response, 200)
	if strings.Contains(response.Body.String(), alice) {
		t.Fatal("cross-member resources leaked")
	}
	// This fixture has no access pool. Mark the zero-side-effect access entries
	// deleted so the test focuses on node cleanup and durable deletion failures.
	if _, e := f.db.SQL.Exec("UPDATE member_access SET invite_state='deleted',key_state='deleted' WHERE member_id=?", alice); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	deleteFail = true
	mu.Unlock()
	requireStatus(t, f.request(t, "DELETE", "/api/members/"+alice, map[string]string{}), 409)
	awaitResource(t, f, alice, nodeID, "failed")
	awaitIdle(t, f, alice)
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", token, nil), 401)
	requireStatus(t, f.request(t, "DELETE", "/api/cluster/nodes/"+nodeID, map[string]string{}), 409)
	mu.Lock()
	deleteFail = false
	mu.Unlock()
	requireStatus(t, f.request(t, "DELETE", "/api/members/"+alice, map[string]string{}), 200)
	for _, table := range []string{"members", "member_access", "member_node_resources", "member_work"} {
		column := "member_id"
		if table == "members" {
			column = "id"
		}
		var count int
		if err := f.db.SQL.QueryRow("SELECT count(*) FROM "+table+" WHERE "+column+"=?", alice).Scan(&count); err != nil || count != 0 {
			t.Fatalf("successful delete retained %s: %d %v", table, count, err)
		}
	}
	requireStatus(t, f.request(t, "GET", "/api/members/"+alice+"/resources", nil), 404)
	if err := f.control.provisionMember(context.Background(), alice); err != nil {
		t.Fatalf("stale queue entry failed after deletion: %v", err)
	}
	response = selfCall(f, "GET", "/api/members/me/resources", bobToken, nil)
	requireStatus(t, response, 200)

}
func TestRegistrationReservesResourcesAtomically(t *testing.T) {
	f := setup(t)
	if _, e := f.db.SQL.Exec(`CREATE TRIGGER fail_member_resources BEFORE INSERT ON member_work BEGIN SELECT RAISE(ABORT,'queue unavailable'); END`); e != nil {
		t.Fatal(e)
	}
	store := &members.Store{Database: f.db}
	i, e := store.CreateInvitation("atomic", 1, "admin")
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.RegisterWith(members.Registration{Password: "Member-password-123", Username: "alice", SSHKey: resourceTestKey, InvitationCode: i.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, f.control.Members.Reserve)
	if e == nil {
		t.Fatal("expected failure")
	}
	for _, table := range []string{"members", "member_access", "member_work"} {
		var count int
		if e = f.db.SQL.QueryRow("SELECT count(*) FROM " + table).Scan(&count); e != nil || count != 0 {
			t.Fatalf("partial %s %d %v", table, count, e)
		}
	}
	var used int
	f.db.SQL.QueryRow("SELECT used FROM member_invitations WHERE id=?", i.ID).Scan(&used)
	if used != 0 {
		t.Fatal("quota consumed")
	}
}

func TestMemberDeletionIncludesWorkersWithoutProvisioningSlots(t *testing.T) {
	f := setup(t)
	id, token := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	if _, err := f.db.SQL.Exec("UPDATE member_access SET invite_state='deleted',key_state='deleted' WHERE member_id=?", id); err != nil {
		t.Fatal(err)
	}
	deleted := false
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		if r.Method != "DELETE" || r.URL.Path != "/api/containers/members/"+id {
			t.Fatalf("unexpected node request: %s %s", r.Method, r.URL.Path)
		}
		deleted = true
		return 200, map[string]bool{"ok": true}, nil
	})
	node, server := worker(t, strings.Repeat("9", 32), Inventory{}, module)
	add(t, f, node, server, "new worker")
	requireStatus(t, f.request(t, "DELETE", "/api/members/"+id, map[string]string{}), 200)
	if !deleted {
		t.Fatal("worker with manually assigned containers was skipped")
	}
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", token, nil), 401)
}
func TestInterruptedQueueRestartsWithoutDuplicatingNodeSlots(t *testing.T) {
	f := setup(t)
	f.control.provisionCancel()
	<-f.control.provisionDone
	// An interrupted pending item must survive close/reopen. No external resources
	// are needed to verify that failures become explicit and the unique slot stays.
	store := &members.Store{Database: f.db}
	invite, _ := store.CreateInvitation("restart", 1, "admin")
	m, e := store.RegisterWith(members.Registration{Password: "Member-password-123", Username: "alice", SSHKey: resourceTestKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, f.control.Members.Reserve)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.db.SQL.Exec("UPDATE member_access SET invite_state='creating' WHERE member_id=?", m.ID); e != nil {
		t.Fatal(e)
	}
	f.control.Close()
	next, e := NewControl(f.db)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	f.control = next
	awaitIdle(t, f, m.ID)
	a, e := next.Bastion.Access(m.ID)
	if e != nil || a.InviteState != "unknown" {
		t.Fatalf("%+v %v", a, e)
	}
	var count int
	if e = f.db.SQL.QueryRow("SELECT count(*) FROM member_work WHERE member_id=?", m.ID).Scan(&count); e != nil || count != 1 {
		t.Fatal(fmt.Sprint(count, e))
	}
}

func TestMemberTokenRotationAndCrossSiteGuards(t *testing.T) {
	f := setup(t)
	id, token := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	requireStatus(t, selfCall(f, "POST", "/api/members/me/token", token, map[string]string{}), 404)
	requireStatus(t, selfCall(f, "POST", "/api/members/"+id+"/token", token, map[string]string{}), 401)
	response := f.request(t, "POST", "/api/members/"+id+"/token", map[string]string{})
	requireStatus(t, response, 200)
	var v map[string]string
	json.Unmarshal(response.Body.Bytes(), &v)
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", token, nil), 401)
	requireStatus(t, selfCall(f, "GET", "/api/members/me/resources", v["resource_token"], nil), 200)
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/members/me/retry", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+v["resource_token"])
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	requireStatus(t, w, 403)
}
