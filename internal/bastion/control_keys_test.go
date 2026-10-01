package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestControlKeyInputs(t *testing.T) {
	var flags controlKeyFlags
	if err := flags.addKey(managerTestKey + " comment"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control.pub")
	if err := os.WriteFile(path, []byte(testKey+" file comment\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := flags.addFile(path); err != nil {
		t.Fatal(err)
	}
	if err := flags.addKey(managerTestKey); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(flags.keys, []string{managerTestKey, testKey}) {
		t.Fatal(flags.keys)
	}
	for _, raw := range []string{"", "bad", managerTestKey + "\n" + testKey, "restrict " + testKey, "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		if err := flags.addKey(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 16385)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := flags.addFile(path); err == nil {
		t.Fatal("accepted oversized file")
	}
	if err := flags.addFile(path + ".missing"); err == nil {
		t.Fatal("accepted missing file")
	}
	var out bytes.Buffer
	if err := InitializeCLI(context.Background(), []string{"--help"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--add-control-key", "--add-control-file", "[--control-key-file FILE]"} {
		if !strings.Contains(out.String(), flag) {
			t.Fatal(out.String())
		}
	}
	for _, args := range [][]string{{"--serve", "--add-control-key", testKey}, {"--uninstall", "--add-control-key", testKey}, {"--add-control-key", testKey, "--status-port", "8765"}} {
		if err := InitializeCLI(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "不接受") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if os.Geteuid() != 0 {
		// These valid forms must reach the privilege check, not demand a key or network options.
		for _, args := range [][]string{{"--listen-host", "100.64.0.2", "--control-url", "http://10.0.0.1:8765"}, {"--add-control-key", testKey}} {
			if err := InitializeCLI(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "sudo") {
				t.Fatalf("%v: %v", args, err)
			}
		}
	}
}
func controlStore(t *testing.T, keys []string) (keyStore, installation) {
	t.Helper()
	s, c := testKeyStore(t)
	c.ControlKeys = keys
	raw, err := marshalInstallation(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.path, "worker_authorized_keys"), controlAuthorizedKeys(keys), 0644); err != nil {
		t.Fatal(err)
	}
	return s, c
}
func TestAppendControlKeysFromEmptyAndRejectMemberUse(t *testing.T) {
	s, c := controlStore(t, []string{})
	r, _, err := s.open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, keys := range [][]string{{managerTestKey}, {testKey, managerTestKey}, {testKey}} {
		if err = appendControlKeys(s, keys, os.WriteFile); err != nil {
			t.Fatal(err)
		}
	}
	current, err := readInstallation(r, c)
	if err != nil || !slices.Equal(current.ControlKeys, []string{managerTestKey, testKey}) {
		t.Fatalf("%+v %v", current, err)
	}
	auth, err := os.ReadFile(filepath.Join(s.path, "worker_authorized_keys"))
	if err != nil || string(auth) != "restrict "+managerTestKey+"\nrestrict "+testKey+"\n" {
		t.Fatalf("%q %v", auth, err)
	}
	// Even an in-flight command holding the old, empty manifest must reject new control keys.
	for _, key := range current.ControlKeys {
		if _, err = updateKeys(r, c, func(v *keySnapshot) error { ensurePoolKeys(v, key); return nil }); err == nil {
			t.Fatal("control key entered member pool")
		}
	}
}
func TestAppendControlKeyConflictAndRollback(t *testing.T) {
	s, _ := controlStore(t, []string{managerTestKey})
	if _, err := s.update(func(v *keySnapshot) error { ensurePoolKeys(v, testKey); return nil }); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(s.path, "installation.json")
	old, _ := os.ReadFile(manifest)
	if err := appendControlKeys(s, []string{testKey}, os.WriteFile); err == nil {
		t.Fatal("member key accepted as control key")
	}
	actual, _ := os.ReadFile(manifest)
	if !bytes.Equal(old, actual) {
		t.Fatal("conflict changed manifest")
	}
	if _, err := s.update(func(v *keySnapshot) error { v.Keys = map[string]string{}; return nil }); err != nil {
		t.Fatal(err)
	}
	failingWrite := func(path string, raw []byte, mode os.FileMode) error {
		if filepath.Base(path) == "worker_authorized_keys" {
			return fmt.Errorf("simulated authorization write failure")
		}
		return os.WriteFile(path, raw, mode)
	}
	if err := appendControlKeys(s, []string{testKey}, failingWrite); err == nil {
		t.Fatal("write failure ignored")
	}
	actual, _ = os.ReadFile(manifest)
	if !bytes.Equal(old, actual) {
		t.Fatal("failed append not rolled back")
	}
	auth, _ := os.ReadFile(filepath.Join(s.path, "worker_authorized_keys"))
	if !bytes.Equal(auth, controlAuthorizedKeys([]string{managerTestKey})) {
		t.Fatal("failed append changed authorization")
	}
}
func TestInstallationRequiresCurrentControlKeyList(t *testing.T) {
	for _, kind := range []string{"old-version", "old-field", "null", "duplicate", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			s, c := controlStore(t, []string{})
			switch kind {
			case "old-version":
				c.Version = 2
			case "null":
				c.ControlKeys = nil
			case "duplicate":
				c.ControlKeys = []string{testKey, testKey}
			case "invalid":
				c.ControlKeys = []string{"bad"}
			}
			raw, _ := json.Marshal(c)
			if kind == "old-field" {
				raw = []byte(strings.Replace(string(raw), `"control_keys":[]`, `"control_key":"`+managerTestKey+`"`, 1))
			}
			if err := os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644); err != nil {
				t.Fatal(err)
			}
			if r, _, err := s.open(); err == nil {
				r.Close()
				t.Fatal("invalid installation accepted")
			}
		})
	}
}
