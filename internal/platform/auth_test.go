package platform

import (
	"encoding/json"
	"testing"

	"project-alpha/internal/testutil"
)

func TestPasswordHash(t *testing.T) {
	const password = "A-test-password-123"
	const salt = "0123456789abcdef0123456789abcdef"
	const expected = salt + ":e1db78576fbf6571fec49ee005002815c70e06479791fc05fb82067b769ebb24"
	if hash, err := PasswordHash(password, salt); err != nil || hash != expected {
		t.Fatalf("PBKDF2 known answer mismatch: %v", err)
	}
	if !CheckPassword(password, expected) || CheckPassword("wrong", expected) {
		t.Fatal("password verification failed")
	}
	for _, malformed := range []string{"", "invalid", "not-hex:hash", expected + ":extra"} {
		if CheckPassword(password, malformed) {
			t.Fatal("accepted malformed password hash")
		}
	}
}

func TestAdministratorSetupWithoutControlAddress(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	client := &testutil.Client{T: t, Handler: NewServer(db, nil, nil, nil, false)}
	for _, field := range []string{"internal_ip", "web_scheme"} {
		client.Expect(400, "POST", "/api/setup", object{"username": "operator", "password": "A-test-password-123", field: "10.0.0.1"}, nil)
	}
	if configured, err := db.Configured(); err != nil || configured {
		t.Fatalf("invalid fields created an administrator: %v %v", configured, err)
	}
	client.Login(true, "operator", "A-test-password-123")
	client.Expect(200, "GET", "/api/session", nil, nil)
	client.Expect(409, "POST", "/api/setup", object{"username": "other", "password": "A-test-password-123"}, nil)
}

func TestAdministratorSetupRollsBackWithAudit(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	if _, err = db.SQL.Exec("CREATE TRIGGER fail_user_audit BEFORE INSERT ON audit WHEN NEW.action='user.create' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateUser(map[string]json.RawMessage{"username": json.RawMessage(`"operator"`), "password": json.RawMessage(`"A-test-password-123"`)}, "setup", true)
	if err == nil {
		t.Fatal("account initialization must fail with the audit")
	}
	if configured, err := db.Configured(); err != nil || configured {
		t.Fatalf("failed setup left an administrator: %v %v", configured, err)
	}
}
