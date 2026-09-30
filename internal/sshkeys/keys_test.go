package sshkeys

import (
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

func TestNormalizeRejectsInjectedOptionsAndMalformedWire(t *testing.T) {
	for _, v := range []string{"", `command="id" ` + testKey, testKey + "\n" + testKey, "ssh-rsa " + strings.Fields(testKey)[1], "ssh-ed25519 AAAA", testKey + "\x00"} {
		if _, e := Normalize(v); e == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	if v, e := Normalize(testKey + " comment"); e != nil || v != testKey {
		t.Fatalf("%q %v", v, e)
	}
}
func TestMemberCommentsPreserveOtherKeys(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	initial := []byte("# existing\n" + testKey + " unrelated\n")
	one, e := Rewrite(initial, a, testKey)
	if e != nil {
		t.Fatal(e)
	}
	both, e := Rewrite(one, b, testKey)
	if e != nil {
		t.Fatal(e)
	}
	again, e := Rewrite(both, a, testKey)
	if e != nil {
		t.Fatal(e)
	}
	removed, e := Rewrite(again, a, "")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(string(removed), string(initial)) || strings.Contains(string(removed), Marker(a)) || !strings.Contains(string(removed), Marker(b)) {
		t.Fatalf("wrong cleanup %s", removed)
	}
	final, e := Rewrite(removed, b, "")
	if e != nil || string(final) != string(initial) {
		t.Fatalf("modified unrelated key %q %v", final, e)
	}
}
