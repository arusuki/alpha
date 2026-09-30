package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestReleasePreservesReaderButStopsPublication(t *testing.T) {
	s, c := keyFixture(t)
	id := strings.Repeat("b", 32)
	if err := s.edit(c.ControlID, id, testKey); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.path, "keys", "keys.json")
	before, _ := os.ReadFile(path)
	if err := os.WriteFile(filepath.Join(s.path, "released"), []byte("released\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", testKey} {
		if err := s.edit(c.ControlID, id, key); err == nil || !strings.Contains(err.Error(), "取消接管") {
			t.Fatalf("released installation accepted publication: %v", err)
		}
	}
	if err := s.checkWriter(c.ControlID); err == nil {
		t.Fatal("released installation reported ready")
	}
	info := s.accountInfo(c.ControlID)
	if !info.Exists || info.Managed || !info.Released {
		t.Fatal(info)
	}
	var output bytes.Buffer
	if err := s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &output); err != nil || !strings.Contains(output.String(), id) {
		t.Fatalf("release revoked existing access: %v %s", err, output.String())
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("release changed data")
	}
	os.Remove(filepath.Join(s.path, "released"))
	if err := s.edit(c.ControlID, id, ""); err != nil {
		t.Fatal("could not revoke after adoption", err)
	}
}

func TestRemovedAccountRetainsValidatedData(t *testing.T) {
	s, c := keyFixture(t)
	c.AccountRemoved, c.Ready = true, false
	os.WriteFile(filepath.Join(s.path, "account-removed"), []byte("account-removed\n"), 0644)
	raw, _ := json.Marshal(c)
	os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644)
	lookup := s.lookup
	s.lookup = func(name string) (*user.User, error) {
		if name == JumpUser {
			return nil, user.UnknownUserError(name)
		}
		return lookup(name)
	}
	r, _, err := s.open()
	if err != nil {
		t.Fatal("could not read retained installation", err)
	}
	r.Close()
	if err = s.checkWriter(c.ControlID); err == nil || !strings.Contains(err.Error(), "已删除") {
		t.Fatal(err)
	}
	if err = s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &bytes.Buffer{}); err == nil {
		t.Fatal("removed account still authorized")
	}
	v := s.accountInfo(c.ControlID)
	if v.Exists || v.Managed || !v.Removed {
		t.Fatal(v)
	}
}

func TestLifecycleMarkersRejectUnsafeState(t *testing.T) {
	for _, kind := range []string{"content", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			s, c := keyFixture(t)
			path := filepath.Join(s.path, "released")
			if err := os.WriteFile(path, []byte("released\n"), 0644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "content":
				os.WriteFile(path, []byte("false"), 0644)
			case "symlink":
				os.Rename(path, path+"-target")
				os.Symlink(path+"-target", path)
			case "permissions":
				os.Chmod(path, 0666)
			}
			if _, _, err := s.open(); err == nil {
				t.Fatal("accepted unsafe lifecycle marker")
			}
			if err := s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &bytes.Buffer{}); err == nil {
				t.Fatal("reader accepted corrupt management state")
			}
		})
	}
}

func TestAccountTransitionsPreserveBinding(t *testing.T) {
	c := installation{ControlID: "control", ServiceUser: "operator", ServiceUID: 1001}
	for _, action := range []string{"init", "release", "delete"} {
		if err := checkInstallationBinding(c, "other", c.ServiceUser, c.ServiceUID, action); err == nil {
			t.Fatalf("%s replaced another control's data", action)
		}
		if err := checkInstallationBinding(c, c.ControlID, "other", c.ServiceUID, action); err == nil {
			t.Fatalf("%s changed service identity", action)
		}
	}
	if err := checkInstallationBinding(c, "other-control", "new-service", 2001, "adopt"); err != nil {
		t.Fatal("explicit adoption must transfer the binding", err)
	}
	c.Released = true
	if err := checkInstallationBinding(c, c.ControlID, c.ServiceUser, c.ServiceUID, "init"); err == nil {
		t.Fatal("reinstall implicitly adopted a released account")
	}
	if err := checkInstallationBinding(c, c.ControlID, c.ServiceUser, c.ServiceUID, "adopt"); err != nil {
		t.Fatal(err)
	}
	c.Released, c.AccountRemoved = false, true
	if err := checkInstallationBinding(c, c.ControlID, c.ServiceUser, c.ServiceUID, "adopt"); err == nil {
		t.Fatal("adopt would create a new account")
	}
	if err := manageAccount(context.Background(), "delete", c, keySnapshot{Keys: map[string]string{"member": testKey}}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "先撤销") {
		t.Fatal("deletion did not reject published keys", err)
	}
}

func TestWebAccountActionsAndConfirmation(t *testing.T) {
	h, _, _ := fixture(t)
	h.installInfo = func() webInstallInfo { return webInstallInfo{ServiceUser: "operator", Available: true} }
	calls := []string{}
	h.installRunner = func(_ context.Context, password []byte, request installRequest) error {
		if request.Directory != h.DB.Directory || request.ServiceUser != "operator" || string(password) != "test-password" {
			t.Fatal("incorrect sudo request")
		}
		calls = append(calls, request.Action)
		return nil
	}
	// Release/delete deliberately make the writer unavailable; that is success.
	h.installation = func() error { return fmt.Errorf("management stopped") }
	call := func(body string) error {
		r := httptest.NewRequest("POST", "/api/bastion/install", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Role: "admin", Username: "admin"})
		return err
	}
	for _, body := range []string{
		`{"sudo_password":"test-password"}`,
		`{"action":"unknown","sudo_password":"test-password"}`,
		`{"action":"delete","sudo_password":"test-password"}`,
		`{"action":"delete","confirm":"root","sudo_password":"test-password"}`,
		`{"action":"adopt","sudo_password":"test-password","directory":"/other"}`,
		`{"action":"release","sudo_password":"test-password","confirm":"alpha-jump"}`,
	} {
		if err := call(body); err == nil {
			t.Fatal("accepted invalid action", body)
		}
	}
	if len(calls) != 0 {
		t.Fatal("invalid request reached sudo")
	}
	for _, action := range []string{"release", "delete", "adopt"} {
		body := map[string]string{"action": action, "sudo_password": "test-password"}
		if action == "delete" {
			body["confirm"] = JumpUser
		}
		if action == "adopt" {
			h.installation = func() error { return nil }
		}
		raw, _ := json.Marshal(body)
		if err := call(string(raw)); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if strings.Join(calls, ",") != "release,delete,adopt" {
		t.Fatal(calls)
	}
	rows, err := platform.Rows(h.DB.SQL, "SELECT action,detail FROM audit WHERE action LIKE 'bastion.account.%'")
	if err != nil || len(rows) != 6 {
		t.Fatal("missing account audit", rows, err)
	}
}
