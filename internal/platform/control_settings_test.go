package platform

import (
	"encoding/json"
	"testing"

	"project-alpha/internal/testutil"
)

func TestControlSetupSettingsAndHostAccess(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	client := &testutil.Client{T: t, Handler: NewServer(db, nil, nil, nil, false)}
	setup := object{"username": "operator", "password": "A-test-password-123"}
	for _, ip := range []any{nil, "", "127.0.0.1", "10.0.0.1:22", "control.example", "::", "ff02::1"} {
		setup["internal_ip"] = ip
		client.Expect(400, "POST", "/api/setup", setup, nil)
	}
	if configured, err := db.Configured(); err != nil || configured {
		t.Fatalf("invalid IP created an administrator: %v %v", configured, err)
	}
	setup["internal_ip"] = "10.0.0.1"
	setup["web_scheme"] = nil
	client.Expect(400, "POST", "/api/setup", setup, nil)
	delete(setup, "web_scheme")
	setup["internal_ip"] = " 100.100.0.1 "
	_, _, response := client.Request("POST", "/api/setup", setup, map[string]string{"Host": "127.0.0.1:8765", "Origin": "http://127.0.0.1:8765"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	client.Login(false, "operator", "A-test-password-123")
	value := client.Expect(200, "GET", "/api/control/settings", nil, nil)
	if value["internal_ip"] != "100.100.0.1" {
		t.Fatalf("bad setup settings: %+v", value)
	}
	client.Expect(200, "GET", "/api/session", nil, map[string]string{"Host": "100.100.0.1:8765"})
	client.Expect(403, "GET", "/api/session", nil, map[string]string{"Host": "100.100.0.2:8765"})
	client.Expect(403, "PUT", "/api/control/settings", value, map[string]string{"X-CSRF-Token": "wrong"})
	for _, invalid := range []object{
		{"internal_ip": ""},
		{"internal_ip": "127.0.0.1"},
		{"internal_ip": "control.example"},
	} {
		invalid["revision"] = 1
		client.Expect(400, "PUT", "/api/control/settings", invalid, nil)
	}
	update := object{"revision": 1, "internal_ip": "fd7a:115c:a1e0::1"}
	saved := client.Expect(200, "PUT", "/api/control/settings", update, nil)
	if saved["revision"] != float64(2) {
		t.Fatal(saved)
	}
	client.Expect(409, "PUT", "/api/control/settings", update, nil)
	client.Expect(403, "GET", "/api/session", nil, map[string]string{"Host": "100.100.0.1:8765"})
	client.Expect(200, "GET", "/api/session", nil, map[string]string{"Host": "[FD7A:115C:A1E0::1]:8443"})
	settings, err := db.ControlSettings()
	if err != nil || settings.InternalIP != "fd7a:115c:a1e0::1" {
		t.Fatalf("invalid control address: %+v %v", settings, err)
	}
	client.Expect(201, "POST", "/api/users", object{"username": "reader", "password": "A-test-password-123", "role": "viewer"}, nil)
	client.Login(false, "reader", "A-test-password-123")
	client.Expect(403, "GET", "/api/control/settings", nil, nil)
	client.Expect(403, "PUT", "/api/control/settings", update, nil)
}

func TestControlSetupRollsBackSettingsWithAccount(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err = db.SQL.Exec("CREATE TRIGGER fail_user_audit BEFORE INSERT ON audit WHEN NEW.action='user.create' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateUser(map[string]json.RawMessage{"username": json.RawMessage(`"operator"`), "password": json.RawMessage(`"A-test-password-123"`), "internal_ip": json.RawMessage(`"10.0.0.1"`)}, "setup", true)
	if err == nil {
		t.Fatal("account initialization must fail with the audit")
	}
	for _, table := range []string{"users", "control_settings"} {
		var count int
		if err = db.SQL.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("setup left %s records: %d %v", table, count, err)
		}
	}
}
