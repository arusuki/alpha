package members

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestMemberPasswordStorageAndSessions(t *testing.T) {
	s := testStore(t)
	_, err := s.SaveSchema(exampleSchema(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := s.CreateInvitation("password", 2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := validRegistration(invitation.Code)
	for _, bad := range []string{"", "short", strings.Repeat("a", 257), "password-with:colon", "password-with\nnewline", "password-with\x00nul"} {
		req.Password = bad
		_, err = s.RegisterWith(req, nil)
		expectError(t, err, 400)
	}
	req.Password = "  Member-password-123  "
	member, err := s.RegisterWith(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hash, ciphertext string
	if err = s.SQL.QueryRow("SELECT password_hash,password_ciphertext FROM members WHERE id=?", member.ID).Scan(&hash, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, req.Password) || strings.Contains(ciphertext, req.Password) || !platform.CheckPassword(req.Password, hash) {
		t.Fatal("unsafe password storage")
	}
	if got, err := s.InitialPassword(member.ID); err != nil || got != req.Password {
		t.Fatalf("initial password %q %v", got, err)
	}
	for _, name := range []string{"alice", "unknown"} {
		_, err = s.LoginStatus(name, "wrong-password")
		expectError(t, err, 401)
	}
	first, err := s.LoginStatus("alice", req.Password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.LoginStatus("alice", req.Password)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.SessionID(first); err != nil || id != member.ID {
		t.Fatalf("session %s %v", id, err)
	}
	expectError(t, s.ChangePassword(member.ID, "wrong-password", "Changed-password-123"), 403)
	if _, err = s.SessionID(first); err != nil {
		t.Fatal("failed change revoked session", err)
	}
	if err = s.ChangePassword(member.ID, req.Password, "Changed-password-123"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first, second, member.ResourceToken} {
		_, err = s.SessionID(token)
		expectError(t, err, 401)
	}
	_, err = s.LoginStatus("alice", req.Password)
	expectError(t, err, 401)
	next, err := s.LoginStatus("alice", "Changed-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.InitialPassword(member.ID); err != nil || got != "Changed-password-123" {
		t.Fatalf("new initial password %q %v", got, err)
	}
	if err = s.Logout(next); err != nil {
		t.Fatal(err)
	}
	_, err = s.SessionID(next)
	expectError(t, err, 401)
	next, err = s.LoginStatus("alice", "Changed-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("UPDATE member_sessions SET expires_at=0"); err != nil {
		t.Fatal(err)
	}
	_, err = s.SessionID(next)
	expectError(t, err, 401)
	if err = os.Remove(filepath.Join(s.Directory, passwordKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.InitialPassword(member.ID); err == nil {
		t.Fatal("missing encryption key ignored")
	}
	if err = s.ChangePassword(member.ID, "Changed-password-123", "Another-password-123"); err == nil {
		t.Fatal("missing key silently replaced")
	}
	req.Username = "other"
	if _, err = s.RegisterWith(req, nil); err == nil {
		t.Fatal("registration replaced missing encryption key")
	}
}

func TestAdminResetPasswordTransactionRollback(t *testing.T) {
	s := testStore(t)
	if _, err := s.SaveSchema(exampleSchema(), "admin"); err != nil {
		t.Fatal(err)
	}
	invitation, err := s.CreateInvitation("reset", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := validRegistration(invitation.Code)
	member, err := s.RegisterWith(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.LoginStatus(req.Username, req.Password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SQL.Exec("CREATE TRIGGER fail_reset_audit BEFORE INSERT ON audit WHEN NEW.action='member.password.reset' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(member.ID, "New-password-123", "operator"); err == nil {
		t.Fatal("reset without audit")
	}
	if _, err = s.SessionID(token); err != nil {
		t.Fatal("failed reset revoked session", err)
	}
	if got, err := s.InitialPassword(member.ID); err != nil || got != req.Password {
		t.Fatal("failed reset changed password", err)
	}
	if _, err = s.LoginStatus(req.Username, "New-password-123"); err == nil {
		t.Fatal("failed reset changed login")
	}
}
