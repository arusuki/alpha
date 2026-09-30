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
	raw, _ := configuredSSH([]byte("HostKey " + key + "\nPasswordAuthentication yes\nAllowTcpForwarding yes\n"))
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
		} else if !strings.Contains(string(out), "passwordauthentication yes\n") {
			t.Fatal("changed other account")
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
