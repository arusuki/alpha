package bastion

import (
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const managerTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH9/f39/f39/f39/f39/f39/f39/f39/f39/f39/f39/"

func testKeyStore(t *testing.T) (keyStore, installation) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("non-root key writer")
	}
	c := installation{Version: installationFormat, ControlKeys: []string{managerTestKey}, ControlURL: "http://10.0.0.1:8765", ListenHost: "100.64.0.2", StatusPort: 9765, JumpUID: os.Geteuid() + 1, JumpGID: os.Getegid(), WorkerUID: os.Geteuid(), WorkerGID: os.Getegid()}
	s := keyStore{path: t.TempDir(), owner: os.Geteuid(), lookup: func(name string) (*user.User, error) {
		uid, gid := c.JumpUID, c.JumpGID
		if name == WorkerUser {
			uid, gid = c.WorkerUID, c.WorkerGID
		}
		return &user.User{Uid: strconv.Itoa(uid), Gid: strconv.Itoa(gid)}, nil
	}}
	if err := os.Chmod(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.path, "keys"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(s.path, "keys"), os.ModeSetgid|0750); err != nil {
		t.Fatal(err)
	}
	return s, c
}
func TestWorkerWritesJumpReadsAndControllerKeyIsIndependent(t *testing.T) {
	s, c := testKeyStore(t)
	for range 2 {
		if _, err := s.update(func(v *keySnapshot) error { ensurePoolKeys(v, testKey); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.snapshot()
	if err != nil || len(v.Keys) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	info, _ := os.Stat(filepath.Join(s.path, "keys", "keys.json"))
	if info.Mode().Perm() != 0640 {
		t.Fatal(info.Mode())
	}
	var out bytes.Buffer
	if err = s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), testKey) || strings.Contains(out.String(), c.ControlKeys[0]) {
		t.Fatal("manager key entered member pool")
	}
	if err = s.authorizedKeys(WorkerUser, strconv.Itoa(c.WorkerUID), &out); err == nil {
		t.Fatal("reader accepted worker")
	}
	if _, err = s.update(func(v *keySnapshot) error { ensurePoolKeys(v, c.ControlKeys[0]); return nil }); err == nil {
		t.Fatal("manager key accepted as member")
	}
}
func TestKeyStoreRejectsChangedIdentityFormatsAndUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"old-version", "unknown-field", "worker-uid", "group", "mode", "symlink", "hardlink", "invalid-key", "missing-version", "missing-keys", "null-snapshot", "control-key", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, c := testKeyStore(t)
			_, err := s.update(func(v *keySnapshot) error { ensurePoolKeys(v, testKey); return nil })
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(s.path, "installation.json")
			keys := filepath.Join(s.path, "keys", "keys.json")
			switch kind {
			case "old-version":
				c.Version = 1
				if err := os.Chmod(s.path, 0700); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(c)
				os.WriteFile(manifest, raw, 0644)
			case "unknown-field":
				raw, _ := os.ReadFile(manifest)
				os.WriteFile(manifest, append(raw[:len(raw)-1], []byte(",\"extra\":true}")...), 0644)
			case "worker-uid":
				s.lookup = func(name string) (*user.User, error) { return &user.User{Uid: "1", Gid: "1"}, nil }
			case "group":
				c.JumpGID++
				if err := os.Chmod(s.path, 0700); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(c)
				os.WriteFile(manifest, raw, 0644)
			case "mode":
				os.Chmod(keys, 0666)
			case "symlink":
				os.Rename(keys, keys+".original")
				os.Symlink(keys+".original", keys)
			case "hardlink":
				os.Link(keys, keys+".copy")
			case "invalid-key":
				os.WriteFile(keys, []byte(`{"version":2,"keys":{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"bad"}}`), 0640)
			case "missing-version":
				os.WriteFile(keys, []byte(`{"keys":{}}`), 0640)
			case "missing-keys":
				os.WriteFile(keys, []byte(`{"version":2}`), 0640)
			case "null-snapshot":
				os.WriteFile(keys, []byte(`null`), 0640)
			case "control-key":
				raw, _ := json.Marshal(keySnapshot{Version: keyFormat, Keys: map[string]string{strings.Repeat("a", 32): testKey, strings.Repeat("b", 32): c.ControlKeys[0]}})
				os.WriteFile(keys, raw, 0640)
			case "lock-symlink":
				lock := filepath.Join(s.path, "keys", ".lock")
				os.Remove(lock)
				os.Symlink(keys, lock)
				_, err = s.update(func(*keySnapshot) error { return nil })
				if err == nil {
					t.Fatal("unsafe lock accepted")
				}
				return
			}
			if _, err = s.snapshot(); err == nil {
				t.Fatal("unsafe store accepted")
			}
			var out bytes.Buffer
			if err = s.authorizedKeys(JumpUser, strconv.Itoa(c.JumpUID), &out); err == nil || out.Len() != 0 {
				t.Fatalf("invalid store emitted authorization: %q %v", out.String(), err)
			}
		})
	}
}
func TestManagementCommandRejectsShellAndMalformedRequests(t *testing.T) {
	s, _ := testKeyStore(t)
	for _, command := range []string{"", "sh", "alpha-worker cmd; id", "alpha-jump cmd", "alpha-worker tunnel"} {
		if err := serveCommand(s, command, strings.NewReader("{}"), &bytes.Buffer{}); err == nil {
			t.Fatal(command)
		}
	}
	for _, request := range []string{`{"version":1,"operation":"inspect"}`, `{"version":2,"operation":"shell"}`, `{"version":2,"operation":"ensure","keys":["bad"]}`, `{"version":2,"operation":"inspect","unknown":1}`, `{"version":2,"operation":"inspect"}{}`} {
		if err := serveCommand(s, "alpha-worker cmd", strings.NewReader(request), &bytes.Buffer{}); err == nil {
			t.Fatal(request)
		}
	}
	for _, request := range []string{`{"version":2,"operation":"ensure","keys":["` + testKey + `"]}`, `{"version":2,"operation":"inspect"}`, `{"version":2,"operation":"remove","key":"` + testKey + `"}`} {
		var output bytes.Buffer
		if err := serveCommand(s, "alpha-worker cmd", strings.NewReader(request), &output); err != nil {
			t.Fatal(err)
		}
		var reply commandReply
		if err := strictJSON(output.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.snapshot()
	if err != nil || len(v.Keys) != 0 {
		t.Fatalf("revocation failed: %+v %v", v, err)
	}
}

func (s keyStore) snapshot() (keySnapshot, error) {
	r, c, err := s.open()
	if err != nil {
		return keySnapshot{}, err
	}
	defer r.Close()
	k, err := openKeys(r, c)
	if err != nil {
		return keySnapshot{}, err
	}
	defer k.Close()
	return loadSnapshot(k, c)
}

func (s keyStore) update(change func(*keySnapshot) error) (keySnapshot, error) {
	r, c, err := s.open()
	if err != nil {
		return keySnapshot{}, err
	}
	defer r.Close()
	return updateKeys(r, c, change)
}
