package members

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type object = map[string]any

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	return &Store{db}
}
func rawProfile(v object) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	json.Unmarshal([]byte(httpapi.JSONText(v)), &out)
	return out
}
func expectError(t *testing.T, err error, code int) {
	t.Helper()
	var api *httpapi.Error
	if !errors.As(err, &api) || api.Status != code {
		t.Fatalf("expected status %d, got %v", code, err)
	}
}
func exampleSchema() Schema {
	return Schema{Revision: 1, Fields: []Field{
		{Key: "full_name", Label: "姓名", Type: "text", Required: true},
		{Key: "degree", Label: "学历", Type: "select", Required: true, Options: []string{"博士", "硕士"}},
		{Key: "group", Label: "组别", Type: "select", Options: []string{"A组", "B组"}},
	}}
}
func validRegistration(code string) Registration {
	return Registration{SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f", Username: "alice", InvitationCode: code, SchemaRevision: 2, Profile: rawProfile(object{"full_name": " 张三 ", "degree": "博士"})}
}
func TestRegistrationQuotaValidationAndPersistence(t *testing.T) {
	s := testStore(t)
	if _, err := s.SaveSchema(exampleSchema(), "operator"); err != nil {
		t.Fatal(err)
	}
	i, err := s.CreateInvitation("A组", 2, "operator")
	if err != nil {
		t.Fatal(err)
	}
	bad := []object{
		{"degree": "博士"}, {"full_name": " ", "degree": "博士"}, {"full_name": 123, "degree": "博士"},
		{"full_name": nil, "degree": "博士"}, {"full_name": "张三", "degree": "本科"},
		{"full_name": "张三", "degree": "博士", "admin": true}, {"full_name": strings.Repeat("名", 513), "degree": "博士"},
		{"full_name": "张三", "degree": "博士", "group": nil}, {"full_name": "张三", "degree": "博士", "group": []string{"A组"}},
	}
	for _, profile := range bad {
		req := validRegistration(i.Code)
		req.Profile = rawProfile(profile)
		_, err = s.RegisterWith(req, nil)
		expectError(t, err, 400)
	}
	req := validRegistration(i.Code)
	req.SchemaRevision = 1
	_, err = s.RegisterWith(req, nil)
	expectError(t, err, 409)
	req = validRegistration(strings.Repeat("0", 48))
	_, err = s.RegisterWith(req, nil)
	expectError(t, err, 400)
	list, _ := s.Invitations()
	if list[0].Used != 0 {
		t.Fatal("failed registrations consumed quota")
	}
	req = validRegistration(i.Code)
	m, err := s.RegisterWith(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Profile["full_name"] != "张三" || m.Schema.Revision != 2 || len(m.ID) != 32 {
		t.Fatalf("invalid member: %+v", m)
	}
	_, err = s.RegisterWith(req, nil)
	expectError(t, err, 409)
	list, _ = s.Invitations()
	if list[0].Used != 1 || list[0].Remaining != 1 {
		t.Fatal("duplicate username consumed quota")
	}
	req.Username = "bob"
	if _, err = s.RegisterWith(req, nil); err != nil {
		t.Fatal(err)
	}
	req.Username = "charlie"
	_, err = s.RegisterWith(req, nil)
	expectError(t, err, 400)
	list, _ = s.Invitations()
	if list[0].Used != 2 || list[0].Remaining != 0 || list[0].Status != "exhausted" || list[0].Code != "" {
		t.Fatalf("bad invitation: %+v", list)
	}
	var count int
	if err = s.SQL.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatalf("registration created platform accounts: %d %v", count, err)
	}
	if _, err = s.SaveSchema(Schema{Revision: 2, Fields: []Field{}}, "operator"); err != nil {
		t.Fatal(err)
	}
	// Reopening preserves the invitation ledger and the schema captured at registration.
	db, err := platform.OpenDatabase(s.Directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	other := &Store{db}
	members, err := other.Members()
	if err != nil || len(members) != 2 || len(members[0].Schema.Fields) != 3 || members[0].Schema.Fields[0].Label != "姓名" {
		t.Fatalf("lost registration snapshot: %+v %v", members, err)
	}
	audit, err := s.AuditRows()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(httpapi.JSONText(audit), i.Code) || strings.Contains(httpapi.JSONText(audit), "张三") {
		t.Fatal("audit leaked invitation code or profile")
	}
	var hash string
	if err = s.SQL.QueryRow("SELECT code_hash FROM member_invitations WHERE id=?", i.ID).Scan(&hash); err != nil || hash == i.Code || hash != hashCode(i.Code) {
		t.Fatal("invitation not hashed")
	}
}

func TestInvitationCodeEncryptionAndAvailability(t *testing.T) {
	s := testStore(t)
	i, err := s.CreateInvitation("共享", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err = s.SQL.QueryRow("SELECT code_ciphertext FROM member_invitations WHERE id=?", i.ID).Scan(&ciphertext); err != nil || strings.Contains(ciphertext, i.Code) {
		t.Fatalf("invitation code not encrypted: %v", err)
	}
	keyPath := filepath.Join(s.Directory, invitationKeyFile)
	db, err := platform.OpenDatabase(s.Directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	code, err := (&Store{db}).InvitationCode(i.ID)
	if err != nil || code != i.Code {
		t.Fatalf("cannot recover code after reopening: %v", err)
	}
	if err = os.Rename(keyPath, keyPath+".saved"); err != nil {
		t.Fatal(err)
	}
	_, err = s.InvitationCode(i.ID)
	expectError(t, err, 500)
	_, err = s.CreateInvitation("missing key", 1, "operator")
	expectError(t, err, 500)
	if _, err = os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("lost key was replaced: %v", err)
	}
	if err = os.Rename(keyPath+".saved", keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterWith(Registration{SSHKey: validRegistration(i.Code).SSHKey, Username: "shared", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = s.InvitationCode(i.ID)
	expectError(t, err, 400)
	i, err = s.CreateInvitation("作废", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeInvitation(i.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	_, err = s.InvitationCode(i.ID)
	expectError(t, err, 400)
}

func TestInvitationCodeIntegrityAndIndependentCreation(t *testing.T) {
	s := testStore(t)
	i, err := s.CreateInvitation("old", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateInvitation("other", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err = s.SQL.QueryRow("SELECT code_ciphertext FROM member_invitations WHERE id=?", i.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("UPDATE member_invitations SET code_ciphertext=(SELECT code_ciphertext FROM member_invitations WHERE id=?) WHERE id=?", other.ID, i.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.InvitationCode(i.ID)
	expectError(t, err, 500)
	if _, err = s.SQL.Exec("UPDATE member_invitations SET code_ciphertext=?,code_hash=? WHERE id=?", ciphertext, hashCode(strings.Repeat("0", 48)), i.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.InvitationCode(i.ID)
	expectError(t, err, 500)
	if !strings.Contains(err.Error(), "摘要与密文不匹配") {
		t.Fatal(err)
	}
	if err = s.RevokeInvitation(i.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("UPDATE member_invitations SET code_ciphertext='broken' WHERE id=?", i.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.CreateInvitation("new", 1, "operator")
	if err != nil {
		t.Fatalf("corrupt retired invitation blocked creation: %v", err)
	}
	code, err := s.InvitationCode(fresh.ID)
	if err != nil || code != fresh.Code {
		t.Fatalf("new invitation cannot be recovered: %v", err)
	}
}

func TestInvitationLabelUpdates(t *testing.T) {
	s := testStore(t)
	for _, status := range []string{"active", "exhausted", "revoked"} {
		t.Run(status, func(t *testing.T) {
			i, err := s.CreateInvitation("原备注", 1, "operator")
			if err != nil {
				t.Fatal(err)
			}
			if status == "exhausted" {
				_, err = s.RegisterWith(Registration{SSHKey: validRegistration(i.Code).SSHKey, Username: "exhausted", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}, nil)
			} else if status == "revoked" {
				err = s.RevokeInvitation(i.ID, "operator")
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.Invitations()
			if err != nil {
				t.Fatal(err)
			}
			var before Invitation
			for _, row := range rows {
				if row.ID == i.ID {
					before = row
				}
			}
			for _, label := range []string{"  新备注  ", strings.Repeat("名", 100), "   "} {
				if err := s.UpdateInvitationLabel(i.ID, label, "editor"); err != nil {
					t.Fatal(err)
				}
				expectError(t, s.UpdateInvitationLabel(i.ID, strings.Repeat("名", 101), "editor"), 400)
				rows, err = s.Invitations()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, after := range rows {
					if after.ID == i.ID {
						found = true
						if after.Label != strings.TrimSpace(label) || after.Status != status {
							t.Fatalf("bad update: %+v", after)
						}
						after.Label = before.Label
						if after != before {
							t.Fatalf("label update changed invitation metadata: %+v -> %+v", before, after)
						}
					}
				}
				if !found {
					t.Fatal("updated invitation disappeared")
				}
			}
		})
	}
	expectError(t, s.UpdateInvitationLabel(strings.Repeat("0", 32), "missing", "editor"), 404)
	var audits int
	if err := s.SQL.QueryRow("SELECT count(*) FROM audit WHERE action='member.invitation.update' AND actor='editor'").Scan(&audits); err != nil || audits != 9 {
		t.Fatalf("missing update audits: %d %v", audits, err)
	}
	i, err := s.CreateInvitation("保留备注", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("CREATE TRIGGER fail_invitation_audit BEFORE INSERT ON audit WHEN NEW.action='member.invitation.update' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateInvitationLabel(i.ID, "不应保存", "editor"); err == nil {
		t.Fatal("expected audit failure")
	}
	var label string
	if err = s.SQL.QueryRow("SELECT label FROM member_invitations WHERE id=?", i.ID).Scan(&label); err != nil || label != i.Label {
		t.Fatalf("failed audit retained label update: %q %v", label, err)
	}
}

func TestInvitationDeletion(t *testing.T) {
	for _, status := range []string{"unused", "active", "exhausted", "revoked"} {
		t.Run(status, func(t *testing.T) {
			s := testStore(t)
			quota := 2
			if status == "exhausted" {
				quota = 1
			}
			i, err := s.CreateInvitation("删除测试", quota, "operator")
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.CreateInvitation("保留", 1, "operator")
			if err != nil {
				t.Fatal(err)
			}
			h := NewHandler(s.Database)
			req := Registration{SSHKey: validRegistration(i.Code).SSHKey, Username: "alice", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}
			token := platform.RandomHex(32)
			var member Member
			if status != "unused" {
				member, err = h.RegisterRegistry(req, token)
				if err != nil {
					t.Fatal(err)
				}
			}
			if status == "revoked" {
				if err = s.RevokeInvitation(i.ID, "operator"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Members()
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DeleteInvitation(i.ID, "editor"); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = s.SQL.QueryRow("SELECT count(*) FROM member_invitations WHERE id=?", i.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invitation was not physically deleted: %d %v", count, err)
			}
			rows, err := s.Invitations()
			if err != nil || len(rows) != 1 || rows[0].ID != other.ID {
				t.Fatalf("deletion changed other invitations: %+v %v", rows, err)
			}
			_, err = s.InvitationCode(i.ID)
			expectError(t, err, 400)
			expectError(t, s.CheckInvitation(i.Code), 400)
			expectError(t, s.DeleteInvitation(i.ID, "editor"), 404)
			_, err = s.RegisterWith(req, nil)
			expectError(t, err, 400)
			if status != "unused" {
				// A committed registry registration remains recoverable after deletion,
				// but its token cannot be reused with different registration content.
				recovered, err := h.RegisterRegistry(req, token)
				if err != nil || httpapi.JSONText(recovered) != httpapi.JSONText(member) || recovered.ResourceToken != token {
					t.Fatalf("lost committed registration: %+v %v", recovered, err)
				}
				req.InvitationCode = other.Code
				_, err = h.RegisterRegistry(req, token)
				expectError(t, err, 409)
				req.InvitationCode, req.Username = i.Code, "bob"
				_, err = h.RegisterRegistry(req, token)
				expectError(t, err, 409)
				_, err = h.RegisterRegistry(req, platform.RandomHex(32))
				expectError(t, err, 400)
			}
			after, err := s.Members()
			if err != nil || httpapi.JSONText(before) != httpapi.JSONText(after) {
				t.Fatalf("deletion changed registered members: %+v %v", after, err)
			}
			var detail string
			if err = s.SQL.QueryRow("SELECT count(*),detail FROM audit WHERE action='member.invitation.delete' AND actor='editor'").Scan(&count, &detail); err != nil || count != 1 || detail != i.ID {
				t.Fatalf("missing deletion audit: %d %q %v", count, detail, err)
			}
			// Reopening also preserves the source ID without needing the deleted row.
			db, err := platform.OpenDatabase(s.Directory, Initialize)
			if err != nil {
				t.Fatal(err)
			}
			defer db.SQL.Close()
			after, err = (&Store{db}).Members()
			if err != nil || httpapi.JSONText(before) != httpapi.JSONText(after) {
				t.Fatalf("lost members after reopening: %+v %v", after, err)
			}
		})
	}
}

func TestInvitationDeletionRollsBackWithoutAudit(t *testing.T) {
	s := testStore(t)
	i, err := s.CreateInvitation("保留", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("CREATE TRIGGER fail_invitation_delete_audit BEFORE INSERT ON audit WHEN NEW.action='member.invitation.delete' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteInvitation(i.ID, "editor"); err == nil {
		t.Fatal("expected audit failure")
	}
	code, err := s.InvitationCode(i.ID)
	if err != nil || code != i.Code {
		t.Fatalf("failed audit retained deletion: %v", err)
	}
}

func TestSchemaValidationAndConflicts(t *testing.T) {
	s := testStore(t)
	cases := []Schema{
		{Revision: 1}, {Revision: 0, Fields: []Field{}},
		{Revision: 1, Fields: []Field{{Key: "name", Label: "名字", Type: "number"}}},
		{Revision: 1, Fields: []Field{{Key: "name", Label: "", Type: "text"}}},
		{Revision: 1, Fields: []Field{{Key: "Bad-Key", Label: "名字", Type: "text"}}},
		{Revision: 1, Fields: []Field{{Key: "name", Label: "名字", Type: "text"}, {Key: "name", Label: "其他", Type: "text"}}},
		{Revision: 1, Fields: []Field{{Key: "degree", Label: "学历", Type: "select"}}},
		{Revision: 1, Fields: []Field{{Key: "degree", Label: "学历", Type: "select", Options: []string{"博士", " 博士 "}}}},
		{Revision: 1, Fields: []Field{{Key: "degree", Label: "学历", Type: "select", Options: []string{" "}}}},
		{Revision: 1, Fields: []Field{{Key: "name", Label: "名字", Type: "text", Options: []string{"选项"}}}},
	}
	for _, value := range cases {
		_, err := s.SaveSchema(value, "operator")
		expectError(t, err, 400)
	}
	saved, err := s.SaveSchema(exampleSchema(), "operator")
	if err != nil || saved.Revision != 2 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	_, err = s.SaveSchema(exampleSchema(), "operator")
	expectError(t, err, 409)
	for _, quota := range []int{-1, 0, 100001} {
		_, err = s.CreateInvitation("", quota, "operator")
		expectError(t, err, 400)
	}
}

func TestConcurrentQuotaAndTransactionalRollback(t *testing.T) {
	s := testStore(t)
	// Independent connections exercise SQLite transaction isolation, not just a Go lock.
	second, err := platform.OpenDatabase(s.Directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer second.SQL.Close()
	stores := []*Store{s, {second}}
	i, err := s.CreateInvitation("concurrent", 3, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for n := range 16 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := stores[n%2].RegisterWith(Registration{SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f", Username: fmt.Sprintf("user%d", n), InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}, nil)
			results <- err
		}(n)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			expectError(t, err, 400)
		}
	}
	if success != 3 {
		t.Fatalf("registered %d, quota 3", success)
	}
	i, err = s.CreateInvitation("rollback", 1, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("CREATE TRIGGER fail_registration_audit BEFORE INSERT ON audit WHEN NEW.action='member.register' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	_, err = s.RegisterWith(Registration{SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f", Username: "rolledback", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}, nil)
	if err == nil {
		t.Fatal("expected audit failure")
	}
	var used, count int
	if err = s.SQL.QueryRow("SELECT used FROM member_invitations WHERE id=?", i.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err = s.SQL.QueryRow("SELECT count(*) FROM members WHERE username='rolledback'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if used != 0 || count != 0 {
		t.Fatal("failed transaction retained member or quota")
	}
	if err = s.RevokeInvitation(i.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	_, err = s.RegisterWith(Registration{SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f", Username: "revoked", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})}, nil)
	expectError(t, err, 400)
	if err = s.RevokeInvitation(i.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Invitations()
	if list[0].Status != "revoked" || list[0].Remaining != 0 {
		t.Fatal("revoked invitation appears usable")
	}
}

func TestHTTPAuthorizationRegistrationAndRateLimit(t *testing.T) {
	s := testStore(t)
	h := NewHandler(s.Database)
	server := platform.NewServer(s.Database, h, web.Assets, nil, false)
	call := func(method, path string, body any, token, csrf string, headers map[string]string) *httptest.ResponseRecorder {
		raw, contentType := httpapi.JSONText(body), "application/json"
		if strings.HasPrefix(path, invitationPage) {
			form := url.Values{}
			if values, ok := body.(object); ok {
				for key, value := range values {
					form.Set(key, fmt.Sprint(value))
				}
			}
			raw, contentType = form.Encode(), "application/x-www-form-urlencoded"
		}
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(raw))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("X-CSRF-Token", csrf)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: token})
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	expect := func(code int, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != code {
			t.Fatalf("want %d, got %d: %s", code, w.Code, w.Body.String())
		}
	}
	setup := call("POST", "/api/setup", object{"username": "operator", "password": "A-test-password-123"}, "", "", nil)
	expect(200, setup)
	var session platform.Session
	if err := json.Unmarshal(setup.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	token := setup.Result().Cookies()[0].Value
	csrf := session.CSRF
	expect(200, call("GET", "/api/members/registration-schema", nil, "", "", nil))
	for _, path := range []string{"/api/members", invitationPage} {
		expect(401, call("GET", path, nil, "", "", nil))
	}
	expect(403, call("PUT", "/api/members/registration-schema", exampleSchema(), token, "", nil))
	expect(200, call("PUT", "/api/members/registration-schema", exampleSchema(), token, csrf, nil))
	expect(400, call("PUT", "/api/members/registration-schema", object{"revision": 2, "fields": []object{{"key": "name", "label": "姓名", "type": "text", "unexpected": true}}}, token, csrf, nil))
	for _, quota := range []any{nil, "invalid", 1.5} {
		expect(400, call("POST", invitationPage+"/create", object{"quota": quota}, token, csrf, nil))
	}
	w := call("POST", invitationPage+"/create", object{"quota": 2, "label": "cohort"}, token, csrf, nil)
	expect(200, w)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal("invitation view is not HTML")
	}
	match := regexp.MustCompile(`data-issued-invitation value="([a-f0-9]{48})"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing one-time invitation: %s", w.Body.String())
	}
	invitations, err := s.Invitations()
	if err != nil {
		t.Fatal(err)
	}
	invitation := invitations[0]
	invitation.Code = match[1]
	keyPath := filepath.Join(s.Directory, invitationKeyFile)
	if err = os.Rename(keyPath, keyPath+".saved"); err != nil {
		t.Fatal(err)
	}
	w = call("POST", invitationPage+"/create", object{"quota": 1}, token, csrf, nil)
	expect(500, w)
	if !strings.Contains(w.Body.String(), "invitation-code.key 丢失") || !strings.Contains(w.Body.String(), "恢复原密钥") || strings.Contains(w.Body.String(), invitation.Code) {
		t.Fatalf("missing actionable key error: %s", w.Body.String())
	}
	if err = os.Rename(keyPath+".saved", keyPath); err != nil {
		t.Fatal(err)
	}
	w = call("GET", invitationPage+"/select", nil, token, csrf, nil)
	expect(200, w)
	if !strings.Contains(w.Body.String(), invitation.ID) || !strings.Contains(w.Body.String(), "cohort") || strings.Contains(w.Body.String(), invitation.Code) {
		t.Fatalf("unsafe invitation selector: %s", w.Body.String())
	}
	update := object{"id": invitation.ID, "label": `  新备注 <img src=x onerror=alert(1)> "  `}
	expect(401, call("POST", invitationPage+"/update", update, "", "", nil))
	expect(403, call("POST", invitationPage+"/update", update, token, "", nil))
	expect(415, call("POST", invitationPage+"/update", update, token, csrf, map[string]string{"Content-Type": "application/json"}))
	for _, invalid := range []object{
		{"id": invitation.ID}, {"label": "missing id"}, {"id": "bad", "label": "bad id"},
		{"id": invitation.ID, "label": strings.Repeat("名", 101)},
		{"id": invitation.ID, "label": "unexpected quota", "quota": 3},
	} {
		expect(400, call("POST", invitationPage+"/update", invalid, token, csrf, nil))
	}
	expect(404, call("POST", invitationPage+"/update", object{"id": strings.Repeat("0", 32), "label": "missing"}, token, csrf, nil))
	w = call("POST", invitationPage+"/update", update, token, csrf, nil)
	expect(200, w)
	if !strings.Contains(w.Body.String(), "新备注 &lt;img") || strings.Contains(w.Body.String(), "<img") || strings.Contains(w.Body.String(), invitation.Code) || strings.Contains(w.Body.String(), "data-issued-invitation") {
		t.Fatalf("unsafe invitation update view: %s", w.Body.String())
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		expect(404, call(method, "/api/members/invitations", object{"quota": 1}, token, csrf, nil))
	}
	expect(403, call("POST", invitationPage+"/create", object{"quota": 1}, token, "", nil))
	expect(415, call("POST", invitationPage+"/create", object{"quota": 1}, token, csrf, map[string]string{"Content-Type": "application/json"}))
	req := validRegistration(invitation.Code)
	expect(403, call("POST", "/api/members/register", req, "", "", map[string]string{"Origin": "https://evil.example"}))
	w = call("POST", "/api/members/register", req, "", "", nil)
	expect(201, w)
	if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "csrf") || strings.Contains(w.Body.String(), "role") {
		t.Fatal("registration minted platform credentials")
	}
	expect(401, call("POST", "/api/login", object{"username": "alice", "password": "A-test-password-123"}, "", "", nil))
	privileged := object{"username": "bob", "invitation_code": invitation.Code, "schema_revision": 2, "profile": object{"full_name": "李四", "degree": "硕士"}, "role": "admin"}
	expect(400, call("POST", "/api/members/register", privileged, "", "", nil))
	delete(privileged, "role")
	privileged["password"] = "unwanted-password"
	expect(400, call("POST", "/api/members/register", privileged, "", "", nil))
	w = call("GET", invitationPage, nil, token, csrf, nil)
	expect(200, w)
	if strings.Contains(w.Body.String(), invitation.Code) || strings.Contains(w.Body.String(), "code_hash") {
		t.Fatal("invitation list leaked code")
	}
	expect(200, call("GET", "/api/members", nil, token, csrf, nil))
	expect(201, call("POST", "/api/users", object{"username": "observer", "password": "A-test-password-123", "role": "viewer"}, token, csrf, nil))
	viewer, vs, err := s.Login("observer", "A-test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/members", invitationPage} {
		expect(403, call("GET", path, nil, viewer, vs.CSRF, nil))
	}
	expect(403, call("PUT", "/api/members/registration-schema", exampleSchema(), viewer, vs.CSRF, nil))
	expect(403, call("POST", invitationPage+"/revoke", object{"id": invitation.ID}, viewer, vs.CSRF, nil))
	expect(403, call("POST", invitationPage+"/update", update, viewer, vs.CSRF, nil))
	deletion := object{"id": invitation.ID}
	expect(401, call("POST", invitationPage+"/delete", deletion, "", "", nil))
	expect(403, call("POST", invitationPage+"/delete", deletion, token, "", nil))
	expect(403, call("POST", invitationPage+"/delete", deletion, viewer, vs.CSRF, nil))
	expect(415, call("POST", invitationPage+"/delete", deletion, token, csrf, map[string]string{"Content-Type": "application/json"}))
	for _, invalid := range []object{{}, {"id": "bad"}, {"id": invitation.ID, "label": "unexpected"}} {
		expect(400, call("POST", invitationPage+"/delete", invalid, token, csrf, nil))
	}
	expect(404, call("POST", invitationPage+"/delete", object{"id": strings.Repeat("0", 32)}, token, csrf, nil))
	expect(404, call("GET", invitationPage+"/delete", deletion, token, csrf, nil))
	expect(200, call("POST", invitationPage+"/revoke", object{"id": invitation.ID}, token, csrf, nil))
	w = call("GET", invitationPage+"/select", nil, token, csrf, nil)
	expect(200, w)
	if strings.Contains(w.Body.String(), invitation.ID) {
		t.Fatal("revoked invitation offered for sharing")
	}
	req.Username = "bob"
	expect(400, call("POST", "/api/members/register", req, "", "", nil))
	w = call("POST", invitationPage+"/delete", deletion, token, csrf, nil)
	expect(200, w)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || strings.Contains(w.Body.String(), invitation.ID) || strings.Contains(w.Body.String(), invitation.Code) || strings.Contains(w.Body.String(), "data-issued-invitation") {
		t.Fatalf("unsafe invitation deletion view: %s", w.Body.String())
	}
	expect(404, call("POST", invitationPage+"/delete", deletion, token, csrf, nil))
	w = call("GET", invitationPage, nil, token, csrf, nil)
	expect(200, w)
	if strings.Contains(w.Body.String(), invitation.ID) {
		t.Fatal("deleted invitation remains in the list")
	}
	w = call("GET", invitationPage+"/select", nil, token, csrf, nil)
	expect(200, w)
	if strings.Contains(w.Body.String(), invitation.ID) {
		t.Fatal("deleted invitation offered for sharing")
	}
	w = call("GET", "/api/members", nil, token, csrf, nil)
	expect(200, w)
	if !strings.Contains(w.Body.String(), invitation.ID) || !strings.Contains(w.Body.String(), "alice") || strings.Contains(w.Body.String(), "invitation_code_hash") {
		t.Fatalf("deletion changed members or leaked private provenance: %s", w.Body.String())
	}
	expect(400, call("POST", "/api/members/register", req, "", "", nil))
	// Registration throttling is independent of platform login attempts.
	h.attempts = map[string]attempt{}
	for range 20 {
		expect(400, call("POST", "/api/members/register", object{}, "", "", nil))
	}
	expect(429, call("POST", "/api/members/register", object{}, "", "", nil))
	expect(200, call("POST", "/api/login", object{"username": "operator", "password": "A-test-password-123"}, "", "", nil))
	for ip, a := range h.attempts {
		a.since = time.Now().Add(-6 * time.Minute)
		h.attempts[ip] = a
	}
	expect(400, call("POST", "/api/members/register", object{}, "", "", nil))
}
