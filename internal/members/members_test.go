package members

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	return Registration{Username: "alice", InvitationCode: code, SchemaRevision: 2, Profile: rawProfile(object{"full_name": " 张三 ", "degree": "博士"})}
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
		_, err = s.Register(req)
		expectError(t, err, 400)
	}
	req := validRegistration(i.Code)
	req.SchemaRevision = 1
	_, err = s.Register(req)
	expectError(t, err, 409)
	req = validRegistration(strings.Repeat("0", 48))
	_, err = s.Register(req)
	expectError(t, err, 400)
	list, _ := s.Invitations()
	if list[0].Used != 0 {
		t.Fatal("failed registrations consumed quota")
	}
	req = validRegistration(i.Code)
	m, err := s.Register(req)
	if err != nil {
		t.Fatal(err)
	}
	if m.Profile["full_name"] != "张三" || m.Schema.Revision != 2 || len(m.ID) != 32 {
		t.Fatalf("invalid member: %+v", m)
	}
	_, err = s.Register(req)
	expectError(t, err, 409)
	list, _ = s.Invitations()
	if list[0].Used != 1 || list[0].Remaining != 1 {
		t.Fatal("duplicate username consumed quota")
	}
	req.Username = "bob"
	if _, err = s.Register(req); err != nil {
		t.Fatal(err)
	}
	req.Username = "charlie"
	_, err = s.Register(req)
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
			_, err := stores[n%2].Register(Registration{Username: fmt.Sprintf("user%d", n), InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})})
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
	_, err = s.Register(Registration{Username: "rolledback", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})})
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
	_, err = s.Register(Registration{Username: "revoked", InvitationCode: i.Code, SchemaRevision: 1, Profile: rawProfile(object{})})
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
	expect(200, call("POST", invitationPage+"/revoke", object{"id": invitation.ID}, token, csrf, nil))
	req.Username = "bob"
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
