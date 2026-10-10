package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/containers"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func TestAdminMemberPasswordResetAndPartialRetry(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	for i, nodeID := range []string{strings.Repeat("8", 32), strings.Repeat("9", 32)} {
		w, s := worker(t, nodeID, Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
			if r.URL.Path == "/api/worker/scheduled-analysis" && r.Method == "GET" {
				return 200, map[string]any{"jobs": []any{}}, nil
			}
			if r.URL.Path == "/api/containers/candidates" && r.Method == "GET" {
				return 200, map[string]any{"containers": []any{}}, nil
			}
			if r.Method != "POST" || r.URL.Path != "/api/containers/members/"+id+"/password" || u.Username != "admin" || u.Role != "admin" {
				t.Errorf("wrong reset %s %s %+v", r.Method, r.URL.Path, u)
			}
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["username"] != "alice" || req["password"] != "New-password-123" {
				t.Errorf("bad reset payload")
			}
			calls.Add(1)
			if i == 1 && fail.Load() {
				return 0, nil, httpapi.NewError(409, "container stopped")
			}
			return 200, containers.PasswordResetResult{OK: true, Updated: 1, Errors: []string{}}, nil
		}))
		add(t, f, w, s, "worker-"+nodeID[:1])
	}
	path := "/api/members/" + id + "/password"
	body := map[string]string{"password": "New-password-123"}
	for _, tok := range []string{"", resourceToken, token} {
		requireStatus(t, selfCall(f, "POST", path, tok, body), 401)
	}
	// A real administrator session still requires CSRF.
	csrf := f.csrf
	f.csrf = ""
	requireStatus(t, f.request(t, "POST", path, body), 403)
	f.csrf = csrf
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"password":"New-password-123"}`))
	if _, _, err := f.control.Dispatch(httptest.NewRecorder(), req, platform.User{Role: "viewer"}); err == nil {
		t.Fatal("viewer allowed")
	}
	requireStatus(t, f.request(t, "POST", path, map[string]string{"password": "short"}), 400)
	gate := f.control.memberGate(id)
	gate.Lock()
	requireStatus(t, f.request(t, "POST", path, body), 409)
	gate.Unlock()
	requireStatus(t, selfCall(f, "GET", "/api/status/alice", token, nil), 200)
	for attempt := range 2 {
		response := f.request(t, "POST", path, body)
		requireStatus(t, response, 200)
		var result struct {
			OK           bool                       `json:"ok"`
			AccountReset bool                       `json:"account_reset"`
			Updated      int                        `json:"updated"`
			Nodes        []memberPasswordNodeResult `json:"nodes"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.AccountReset || result.OK != (attempt == 1) || result.Updated != attempt+1 || len(result.Nodes) != 2 {
			t.Fatalf("wrong result %s", response.Body.String())
		}
		if strings.Contains(response.Body.String(), body["password"]) {
			t.Fatal("password leaked")
		}
		if attempt == 0 && !strings.Contains(response.Body.String(), "container stopped") {
			t.Fatal("failure hidden")
		}
		requireStatus(t, selfCall(f, "GET", "/api/status/alice", token, nil), 401)
		store := &members.Store{Database: f.db}
		if _, err := store.LoginStatus("alice", "Member-password-123"); err == nil {
			t.Fatal("old password accepted")
		}
		var err error
		token, err = store.LoginStatus("alice", body["password"])
		if err != nil {
			t.Fatal(err)
		}
		if password, err := store.InitialPassword(id); err != nil || password != body["password"] {
			t.Fatal("provisioning password not updated", err)
		}
		fail.Store(false)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls %d", calls.Load())
	}
	var actor, detail string
	if err := f.db.SQL.QueryRow("SELECT actor,detail FROM audit WHERE action='member.password.reset'").Scan(&actor, &detail); err != nil || actor != "admin" || detail != id {
		t.Fatalf("audit %s %s %v", actor, detail, err)
	}
	requireStatus(t, f.request(t, "POST", "/api/members/"+strings.Repeat("f", 32)+"/password", body), 404)
	requireStatus(t, f.request(t, "GET", path, nil), 405)
}
