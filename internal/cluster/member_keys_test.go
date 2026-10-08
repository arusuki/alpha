package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

const secondMemberKey = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBIvR3cOir2XFsX4NiA4QO1JKQ7c87emaiV0rBXS3fiseEt0seHFTvuv2Tl0Zz5jQJS1Ko0oVLFAQZ4BtLtn6hKg="

func TestStatusMultipleKeysSaveIsolationFailureAndRetry(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	bob, _ := registerResource(t, f, "bob")
	awaitIdle(t, f, bob)
	bobToken := statusLogin(t, f, "bob", "Member-password-123")
	var fail atomic.Bool
	var calls atomic.Int32
	fail.Store(true)
	keys := resourceTestKey + "\n" + secondMemberKey
	w, s := worker(t, strings.Repeat("8", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if r.URL.Path == "/api/containers/candidates" {
			return 200, map[string]any{"containers": []any{}}, nil
		}
		if r.Method != "PATCH" || r.URL.Path != "/api/containers/members/"+id || u.ID != id {
			t.Errorf("wrong dispatch: %s %s %+v", r.Method, r.URL.Path, u)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["ssh_public_key"] != keys {
			t.Errorf("wrong keys: %v %v", req, err)
		}
		calls.Add(1)
		if fail.Load() {
			return 0, nil, httpapi.NewError(409, "container offline")
		}
		return 200, map[string]bool{"ok": true}, nil
	}))
	add(t, f, w, s, "Keys worker")
	path := "/api/status/alice/keys"
	for _, tok := range []string{"", resourceToken, bobToken} {
		want := 401
		if tok == bobToken {
			want = 403
		}
		requireStatus(t, selfCall(f, "POST", path, tok, map[string]string{"ssh_public_key": keys}), want)
	}
	for _, bad := range []string{"", resourceTestKey + "\ninvalid"} {
		requireStatus(t, selfCall(f, "POST", path, token, map[string]string{"ssh_public_key": bad}), 400)
	}
	requireStatus(t, selfCall(f, "POST", path, token, map[string]string{"ssh_public_key": keys + "\n" + resourceTestKey + " duplicate"}), 202)
	awaitIdle(t, f, id)
	var state, message string
	if err := f.db.SQL.QueryRow("SELECT state,error FROM member_key_sync WHERE member_id=? AND node_id=?", id, w.ID).Scan(&state, &message); err != nil || state != "failed" || message != "container offline" {
		t.Fatalf("lost failure: %s %s %v", state, message, err)
	}
	requireStatus(t, selfCall(f, "GET", "/api/status/alice", token, nil), 200)
	view := selfCall(f, "GET", "/api/status/alice", token, nil)
	var data struct {
		Keys  string             `json:"ssh_public_key"`
		Nodes []memberNodeStatus `json:"nodes"`
	}
	if err := json.Unmarshal(view.Body.Bytes(), &data); err != nil || data.Keys != keys || len(data.Nodes) != 1 || data.Nodes[0].KeyState != "failed" {
		t.Fatalf("status: %s %v", view.Body.String(), err)
	}
	fail.Store(false)
	requireStatus(t, selfCall(f, "POST", path, token, map[string]string{"ssh_public_key": keys}), 202)
	awaitIdle(t, f, id)
	if err := f.db.SQL.QueryRow("SELECT state FROM member_key_sync WHERE member_id=?", id).Scan(&state); err != nil || state != "ready" || calls.Load() != 2 {
		t.Fatalf("retry: %s %d %v", state, calls.Load(), err)
	}
}

func TestMultipleKeyRegistrationAndUpgradePreservesExistingData(t *testing.T) {
	dir := t.TempDir()
	db, err := platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	store := &members.Store{Database: db}
	invitation, err := store.CreateInvitation("keys", 2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: resourceTestKey, InvitationCode: invitation.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}
	first, err := store.RegisterWith(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("DROP TABLE member_key_sync; DROP TABLE member_key_revocations; DROP TABLE mihomo_profiles; DROP TABLE mihomo_sync; DROP TABLE mihomo_runtime; PRAGMA user_version=34"); err != nil {
		t.Fatal(err)
	}
	db.SQL.Close()
	db, err = platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	store = &members.Store{Database: db}
	var key string
	if err = db.SQL.QueryRow("SELECT ssh_public_key FROM members WHERE id=?", first.ID).Scan(&key); err != nil || key != resourceTestKey {
		t.Fatalf("lost key: %s %v", key, err)
	}
	if _, err = members.NewHandler(db).RegisterRegistry(req, first.ResourceToken); err != nil {
		t.Fatal("lost resource token", err)
	}
	req.Username = "bravo"
	req.SSHKey = resourceTestKey + " laptop\r\n" + secondMemberKey + " desktop\n" + resourceTestKey
	second, err := store.RegisterWith(req, nil)
	if err != nil || second.SSHKey != resourceTestKey+"\n"+secondMemberKey {
		t.Fatalf("multi registration: %+v %v", second, err)
	}
	var version int
	if err = db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != platform.DatabaseVersion {
		t.Fatal(version, err)
	}
	// Reopening is idempotent; neither migration table should be re-created.
	db.SQL.Close()
	db, err = platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
}
