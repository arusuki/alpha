package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHInstallAndUninstallPreserveOtherAccounts(t *testing.T) {
	original := []byte("Port 2222\nMatch User operator\n    AllowTcpForwarding no\n")
	once, err := configuredSSH(original)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := configuredSSH(once)
	if err != nil || !bytes.Equal(once, twice) {
		t.Fatalf("not idempotent: %v", err)
	}
	restored, err := removeManagedSSH(once)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("uninstall changed original: %s %v", restored, err)
	}
	for _, name := range []string{JumpUser, WorkerUser} {
		if strings.Count(string(once), "Match User "+name) != 1 {
			t.Fatal(string(once))
		}
	}
	for _, bad := range []string{configBegin, configEnd, configEnd + configBegin, string(once) + "Port 22\n"} {
		if _, err := removeManagedSSH([]byte(bad)); err == nil {
			t.Fatal("accepted bad markers")
		}
	}
	for _, ending := range []string{"", "\n", "\n\n"} {
		original := []byte("Port 2222" + ending)
		installed, err := configuredSSH(original)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := removeManagedSSH(installed)
		if err != nil || !bytes.Equal(restored, original) {
			t.Fatalf("uninstall changed trailing newlines: %q %v", restored, err)
		}
	}
}
func TestOpenSSHPoliciesForBothNonRootAccounts(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd unavailable")
	}
	key := os.Getenv("PROJECT_ALPHA_SSH_HOST_KEY")
	if key == "" {
		t.Skip("set PROJECT_ALPHA_SSH_HOST_KEY to an existing disposable SSH host key")
	}
	dir := t.TempDir()
	raw, _ := configuredSSH([]byte("HostKey " + key + "\nPasswordAuthentication yes\nAllowTcpForwarding yes\nTrustedUserCAKeys " + key + ".pub\nDisableForwarding yes\nPermitOpen 10.0.0.99:22\n"))
	path := filepath.Join(dir, "sshd_config")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{JumpUser, WorkerUser, "operator", "root"} {
		out, err := exec.Command(sshd, "-T", "-f", path, "-C", "user="+name+",host=localhost,addr=127.0.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if name == JumpUser || name == WorkerUser {
			if err = checkEffectiveSSH(out, name); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, expected := range []string{"passwordauthentication yes\n", "trustedusercakeys " + key + ".pub\n", "disableforwarding yes\n", "permitopen 10.0.0.99:22\n"} {
				if !strings.Contains(string(out), expected) {
					t.Fatalf("changed other account %s: missing %q", name, expected)
				}
			}
		}
	}
	conflicting := append([]byte("HostKey "+key+"\nMatch User alpha-worker\n AllowTcpForwarding yes\n"), []byte(jumpSSHConfig())...)
	os.WriteFile(path, conflicting, 0600)
	out, err := exec.Command(sshd, "-T", "-f", path, "-C", "user=alpha-worker,host=localhost,addr=127.0.0.1").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if checkEffectiveSSH(out, WorkerUser) == nil {
		t.Fatal("accepted conflicting policy")
	}
	for _, policy := range []string{"TrustedUserCAKeys " + key + ".pub", "DisableForwarding yes", "PermitOpen none"} {
		conflicting := []byte("HostKey " + key + "\nMatch User alpha-jump\n " + policy + "\n" + jumpSSHConfig())
		if err = os.WriteFile(path, conflicting, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sshd, "-T", "-f", path, "-C", "user=alpha-jump,host=localhost,addr=127.0.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if checkEffectiveSSH(out, JumpUser) == nil {
			t.Fatalf("accepted conflicting jump policy: %s", policy)
		}
	}
	if strings.Contains(string(proxyUnit()), "User=root") || !strings.Contains(string(proxyUnit()), "User=alpha-worker") {
		t.Fatal("proxy must run as worker")
	}
}
func TestShareCLIRequiresExplicitConfiguration(t *testing.T) {
	for _, args := range [][]string{{}, {"--uninstall", "--control-url", "http://10.0.0.1"}, {"--control-key-file", "key"}, {"--serve", "--uninstall"}, {"--serve", "--status-port", "9765"}, {"--serve", "--no-service"}, {"--uninstall", "--status-port", "9765"}, {"unexpected"}} {
		if err := InitializeCLI(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal(args)
		}
	}
}

func TestUninstallReadsIdentitiesWithoutAcceptingRuntimeFormats(t *testing.T) {
	for _, kind := range []string{"ready", "old-version", "old-control-key", "invalid-proxy", "invalid-control-keys"} {
		t.Run(kind, func(t *testing.T) {
			s, c := testKeyStore(t)
			path := filepath.Join(s.path, "installation.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "ready":
				fields["ready"] = json.RawMessage(`true`)
			case "old-version":
				fields["version"] = json.RawMessage(`1`)
			case "old-control-key":
				delete(fields, "control_keys")
				fields["control_key"] = json.RawMessage(`"obsolete"`)
			case "invalid-proxy":
				fields["control_url"] = json.RawMessage(`"invalid"`)
			case "invalid-control-keys":
				fields["control_keys"] = json.RawMessage(`null`)
			}
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			if r, _, err := s.open(); err == nil {
				r.Close()
				t.Fatal("runtime accepted invalid format")
			}
			r, ids, err := s.openUninstallManifest()
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			if ids.JumpUID != c.JumpUID || ids.JumpGID != c.JumpGID || ids.WorkerUID != c.WorkerUID || ids.WorkerGID != c.WorkerGID {
				t.Fatalf("wrong removal identities: %+v", ids)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("manifest changed: %v", err)
			}
		})
	}
}

func TestUninstallRejectsInvalidOrUnsafeIdentityRecords(t *testing.T) {
	for _, kind := range []string{"missing-jump-uid", "missing-jump-gid", "missing-worker-uid", "missing-worker-gid", "null-id", "zero-id", "negative-id", "string-id", "fractional-id", "same-uid", "null", "malformed", "trailing-json", "mode", "directory-mode", "symlink", "hardlink", "directory-symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testKeyStore(t)
			path := filepath.Join(s.path, "installation.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "null-id", "zero-id", "negative-id", "string-id", "fractional-id":
				fields["worker_uid"] = map[string]json.RawMessage{"null-id": json.RawMessage(`null`), "zero-id": json.RawMessage(`0`), "negative-id": json.RawMessage(`-1`), "string-id": json.RawMessage(`"1000"`), "fractional-id": json.RawMessage(`1000.5`)}[kind]
			case "same-uid":
				fields["worker_uid"] = fields["jump_uid"]
			default:
				if strings.HasPrefix(kind, "missing-") {
					delete(fields, strings.ReplaceAll(strings.TrimPrefix(kind, "missing-"), "-", "_"))
				}
			}
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "null":
				raw = []byte(`null`)
			case "malformed":
				raw = []byte(`{`)
			case "trailing-json":
				raw = append(raw, []byte(`{}`)...)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mode":
				err = os.Chmod(path, 0666)
			case "directory-mode":
				err = os.Chmod(s.path, 0777)
			case "symlink":
				if err = os.Rename(path, path+".original"); err == nil {
					err = os.Symlink(path+".original", path)
				}
			case "hardlink":
				err = os.Link(path, path+".copy")
			case "directory-symlink":
				link := filepath.Join(t.TempDir(), "installation")
				err = os.Symlink(s.path, link)
				s.path = link
			}
			if err != nil {
				t.Fatal(err)
			}
			// Removal must not infer missing or invalid IDs from a valid account lookup.
			if r, _, err := s.openUninstallManifest(); err == nil {
				r.Close()
				t.Fatal("accepted unsafe removal record")
			}
		})
	}
}

func TestUninstallKeyFilesIgnoresDataFormatAndChecksFileIdentity(t *testing.T) {
	for _, kind := range []string{"old-format", "malformed", "unknown-file", "symlink", "hardlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			s, c := testKeyStore(t)
			r, _, err := s.openUninstallManifest()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			k, err := openKeys(r, c)
			if err != nil {
				t.Fatal(err)
			}
			defer k.Close()
			path := filepath.Join(s.path, "keys", "keys.json")
			raw := []byte(`{"version":1,"keys":{}}`)
			if kind == "malformed" {
				raw = []byte(`{`)
			}
			if err := os.WriteFile(path, raw, 0640); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unknown-file":
				err = os.WriteFile(filepath.Join(s.path, "keys", "extra"), raw, 0640)
			case "symlink":
				if err = os.Remove(path); err == nil {
					err = os.Symlink(filepath.Join(s.path, "installation.json"), path)
				}
			case "hardlink":
				err = os.Link(path, filepath.Join(t.TempDir(), "copy"))
			case "mode":
				err = os.Chmod(path, 0666)
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := uninstallKeyEntries(k, c)
			if kind == "old-format" || kind == "malformed" {
				if err != nil || len(entries) != 1 {
					t.Fatalf("explicit removal blocked by key data: %v", err)
				}
				if _, err := loadSnapshot(k, c); err == nil {
					t.Fatal("runtime accepted invalid key data")
				}
			} else if err == nil {
				t.Fatal("unsafe key file accepted for removal")
			}
		})
	}
}
