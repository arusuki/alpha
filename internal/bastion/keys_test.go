package bastion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func keyFixture(t *testing.T) (keyStore, installation) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("publisher is intentionally non-root")
	}
	c := installation{Version: keyFormat, Ready: true, ControlID: strings.Repeat("a", 32), ServiceUser: "control", ServiceUID: os.Geteuid(), JumpUID: os.Geteuid() + 1, JumpGID: os.Getegid()}
	s := keyStore{path: t.TempDir(), owner: os.Geteuid(), lookup: func(name string) (*user.User, error) {
		if name == "control" {
			return &user.User{Uid: strconv.Itoa(c.ServiceUID)}, nil
		}
		if name == JumpUser {
			return &user.User{Uid: strconv.Itoa(c.JumpUID), Gid: strconv.Itoa(c.JumpGID)}, nil
		}
		return nil, fmt.Errorf("unknown user")
	}}
	if err := os.Chmod(s.path, 0755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(s.path, "keys")
	if err := os.Mkdir(keys, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keys, os.ModeSetgid|0750); err != nil {
		t.Fatal(err)
	}
	return s, c
}

// setPoolEntry seeds or removes a pool entry without any member-ID mapping.
func setPoolEntry(s keyStore, control, entry, key string) error {
	return s.update(control, func(v *keySnapshot) error {
		if key == "" {
			delete(v.Keys, entry)
		} else {
			v.Keys[entry] = key
		}
		return nil
	})
}

func TestKeySnapshotPublicationAndRevocation(t *testing.T) {
	s, c := keyFixture(t)
	a, b := strings.Repeat("b", 32), strings.Repeat("c", 32)
	for _, id := range []string{a, b} {
		if err := setPoolEntry(s, c.ControlID, id, testKey); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), keyOptions) != 2 {
		t.Fatal(out.String())
	}
	if err := setPoolEntry(s, c.ControlID, a, ""); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), a) || !strings.Contains(out.String(), b) {
		t.Fatal("removed another pool entry")
	}
	if err := setPoolEntry(s, c.ControlID, b, ""); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out); err != nil || out.Len() != 0 {
		t.Fatalf("revocation failed: %v %s", err, out.String())
	}
	info, err := os.Stat(filepath.Join(s.path, "keys", "keys.json"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("snapshot permissions: %v %v", info, err)
	}
}

func TestKeyStoreRejectsIdentityAndUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"control", "service", "jump", "version", "incomplete", "symlink", "hardlink", "fifo", "directory-mode", "file-mode", "invalid-key", "invalid-entry", "missing-version", "missing-control", "snapshot-version", "wrong-account", "wrong-uid"} {
		t.Run(kind, func(t *testing.T) {
			s, c := keyFixture(t)
			id := strings.Repeat("b", 32)
			if err := setPoolEntry(s, c.ControlID, id, testKey); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.path, "keys", "keys.json")
			before, _ := os.ReadFile(path)
			var out bytes.Buffer
			var err error
			switch kind {
			case "control":
				err = setPoolEntry(s, strings.Repeat("d", 32), id, "")
			case "service", "jump":
				lookup := s.lookup
				s.lookup = func(name string) (*user.User, error) {
					u, e := lookup(name)
					if name == "control" && kind == "service" || name == JumpUser && kind == "jump" {
						u.Uid = "12345678"
					}
					return u, e
				}
				err = setPoolEntry(s, c.ControlID, id, "")
			case "version", "incomplete":
				if kind == "version" {
					c.Version++
				} else {
					c.Ready = false
				}
				raw, _ := json.Marshal(c)
				os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644)
				err = setPoolEntry(s, c.ControlID, id, "")
			case "symlink", "hardlink", "fifo":
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				outside := filepath.Join(t.TempDir(), "outside")
				os.WriteFile(outside, before, 0640)
				if kind == "symlink" {
					err = os.Symlink(outside, path)
				} else if kind == "hardlink" {
					err = os.Link(outside, path)
				} else {
					err = syscall.Mkfifo(path, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				err = setPoolEntry(s, c.ControlID, id, "")
				after, _ := os.ReadFile(outside)
				if !bytes.Equal(before, after) {
					t.Fatal("modified external file")
				}
			case "directory-mode":
				os.Chmod(filepath.Dir(path), 0770)
				err = setPoolEntry(s, c.ControlID, id, "")
			case "file-mode":
				os.Chmod(path, 0660)
				err = setPoolEntry(s, c.ControlID, id, "")
			case "invalid-key", "invalid-entry", "missing-version", "missing-control", "snapshot-version":
				var fields map[string]any
				json.Unmarshal(before, &fields)
				switch kind {
				case "invalid-key":
					fields["keys"].(map[string]any)[id] = "command=\"/bin/sh\" " + testKey
				case "invalid-entry":
					fields["keys"].(map[string]any)["../root"] = testKey
				case "snapshot-version":
					fields["version"] = keyFormat + 1
				case "missing-version":
					delete(fields, "version")
				case "missing-control":
					delete(fields, "control_id")
				}
				raw, _ := json.Marshal(fields)
				os.WriteFile(path, raw, 0640)
				err = s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out)
			case "wrong-account":
				err = s.authorizedKeys("root", "0", &out)
			case "wrong-uid":
				err = s.authorizedKeys(JumpUser, "0", &out)
			}
			if err == nil || out.Len() != 0 {
				t.Fatalf("accepted %s: %v %s", kind, err, out.String())
			}
		})
	}
}

func TestKeyStoreLockAndFailedUpdatePreserveSnapshot(t *testing.T) {
	s, c := keyFixture(t)
	id := strings.Repeat("b", 32)
	if err := setPoolEntry(s, c.ControlID, id, testKey); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.path, "keys", "keys.json")
	before, _ := os.ReadFile(path)
	f, err := os.OpenFile(filepath.Join(s.path, "keys", ".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = setPoolEntry(s, c.ControlID, id, ""); err == nil {
		t.Fatal("ignored concurrent writer")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err = s.update(c.ControlID, func(v *keySnapshot) error {
		delete(v.Keys, id)
		return fmt.Errorf("publication rejected")
	}); err == nil {
		t.Fatal("ignored rejected update")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed write changed snapshot")
	}
}
