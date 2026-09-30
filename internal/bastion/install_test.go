package bastion

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigurationIdempotentAndScoped(t *testing.T) {
	original := []byte("Port 2222\nMatch User operator\n    AllowTcpForwarding no\n")
	once, err := configuredSSH(original)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := configuredSSH(once)
	if err != nil || !bytes.Equal(once, twice) || !bytes.HasPrefix(once, original) {
		t.Fatalf("changed existing config: %v", err)
	}
	if strings.Count(string(once), "Match User alpha-jump") != 1 {
		t.Fatal(string(once))
	}
	for _, bad := range []string{configBegin, configEnd, configEnd + configBegin, string(once) + "Port 22\n"} {
		if _, err := configuredSSH([]byte(bad)); err == nil {
			t.Fatalf("accepted malformed markers %q", bad)
		}
	}
}

func TestInstalledSSHPolicyWithOpenSSH(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("OpenSSH server is not installed")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "host-key")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	config := filepath.Join(dir, "sshd_config")
	raw, err := configuredSSH([]byte("HostKey " + key + "\nPasswordAuthentication yes\nAllowTcpForwarding yes\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{JumpUser, "operator", "root"} {
		out, err := exec.Command(sshd, "-T", "-f", config, "-C", "user="+name+",host=localhost,addr=127.0.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if name == JumpUser {
			if err = checkEffectiveSSH(out); err != nil {
				t.Fatal(err)
			}
		} else if !strings.Contains(string(out), "passwordauthentication yes\n") || !strings.Contains(string(out), "authorizedkeyscommand none\n") {
			t.Fatalf("changed authentication of %s", name)
		}
	}
	conflicting := append([]byte("HostKey "+key+"\nMatch User alpha-jump\n MaxSessions 10\n"), []byte(jumpSSHConfig())...)
	os.WriteFile(config, conflicting, 0600)
	out, err := exec.Command(sshd, "-T", "-f", config, "-C", "user=alpha-jump,host=localhost,addr=127.0.0.1").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err = checkEffectiveSSH(out); err == nil {
		t.Fatal("accepted an earlier conflicting Match")
	}
}

func TestInitializationRequiresRootAndExplicitIdentity(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{"init"}, {"init", "--service-user", "operator"}, {"init", "--service-user", "operator", "--data-dir", "/tmp/control", "unexpected"}} {
		if err := CLI(context.Background(), args, &out); err == nil {
			t.Fatal("accepted missing or ambiguous install arguments")
		}
	}
	if os.Geteuid() != 0 {
		err := CLI(context.Background(), []string{"init", "--service-user", "operator", "--data-dir", "/tmp/control"}, &out)
		if err == nil || !strings.Contains(err.Error(), "初始化需要 sudo") {
			t.Fatal(err)
		}
	}
}
