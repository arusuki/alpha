package platform

import (
	"testing"
)

func TestPasswordHash(t *testing.T) {
	const password = "A-test-password-123"
	const salt = "0123456789abcdef0123456789abcdef"
	const expected = salt + ":e1db78576fbf6571fec49ee005002815c70e06479791fc05fb82067b769ebb24"
	if hash, err := passwordHash(password, salt); err != nil || hash != expected {
		t.Fatalf("PBKDF2 known answer mismatch: %v", err)
	}
	if !checkPassword(password, expected) || checkPassword("wrong", expected) {
		t.Fatal("password verification failed")
	}
	for _, malformed := range []string{"", "invalid", "not-hex:hash", expected + ":extra"} {
		if checkPassword(password, malformed) {
			t.Fatal("accepted malformed password hash")
		}
	}
}
