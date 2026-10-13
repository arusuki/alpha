package containers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func cliFixture(t *testing.T) (*Handler, *fakeDocker, string, string) {
	t.Helper()
	h, docker, _ := fixture(t)
	adopt(t, h)
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET initialized=1,owner='alice'"); err != nil {
		t.Fatal(err)
	}
	h.chatBinary = func() ([]byte, error) { return []byte("#!/bin/sh\n[ \"$1\" = --version ]\n"), nil }
	h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if args[0] == "exec" && slices.Contains(args, installCLIScript) {
			// Execute only the installer in a test-owned directory. No Docker daemon.
			i := slices.Index(args, "/bin/sh")
			cmd := exec.CommandContext(ctx, args[i], args[i+1:]...)
			cmd.Stdin = strings.NewReader(input)
			raw, err := cmd.CombinedOutput()
			return string(raw), err
		}
		return docker.run(ctx, endpoint, args, input)
	}
	return h, docker, "/api/containers/members/" + strings.Repeat("b", 32) + "/cli", filepath.Join(t.TempDir(), "bin", "ops-chat")
}

func putCLI(t *testing.T, h *Handler, path, dest string) cliResult {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"server": "https://control.test", "username": "alice", "command_path": dest})
	code, body := call(h, "PUT", path, string(raw), admin)
	if code != 200 {
		t.Fatal(code, body)
	}
	var out cliResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCLIDeploymentInstallsDefaultsRetriesAndRenames(t *testing.T) {
	h, _, route, path := cliFixture(t)
	for range 2 {
		if out := putCLI(t, h, route, path); !out.OK || out.Installed != 1 {
			t.Fatal(out)
		}
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]string
	if err = json.Unmarshal(raw, &cfg); err != nil || len(cfg) != 2 || cfg["username"] != "alice" || cfg["server"] != "https://control.test" {
		t.Fatal(cfg, err)
	}
	renamed := filepath.Join(filepath.Dir(filepath.Dir(path)), "newbin", "ask")
	if out := putCLI(t, h, route, renamed); !out.OK || out.Installed != 1 {
		t.Fatal(out)
	}
	for _, old := range []string{path, filepath.Dir(target)} {
		if _, err = os.Lstat(old); !os.IsNotExist(err) {
			t.Fatal("old managed install retained", old, err)
		}
	}
	if err = h.reconcileCLI(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(renamed); err != nil {
		t.Fatal(err)
	}
}

func TestCLIDeploymentFailurePreservesFilesAndDesiredConfig(t *testing.T) {
	h, docker, route, path := cliFixture(t)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user-owned"), 0755); err != nil {
		t.Fatal(err)
	}
	if out := putCLI(t, h, route, path); out.OK || len(out.Errors) != 1 {
		t.Fatal(out)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "user-owned" {
		t.Fatal("unrelated file overwritten")
	}
	var installed, desired int
	h.db.SQL.QueryRow("SELECT count(*) FROM container_cli_installs").Scan(&installed)
	h.db.SQL.QueryRow("SELECT count(*) FROM member_cli_configs").Scan(&desired)
	if installed != 0 || desired != 1 {
		t.Fatal(installed, desired)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	docker.c.State.Running = false
	if err := h.reconcileCLI(context.Background(), docker.c.ID); err == nil {
		t.Fatal("stopped container accepted")
	}
	docker.c.State.Running = true
	if err := h.reconcileCLI(context.Background(), docker.c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("saved config was not retried", err)
	}
	// A changed parent must not let rename cleanup traverse outside the old location.
	oldParent := filepath.Dir(path)
	if err := os.Rename(oldParent, oldParent+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldParent+"-moved", oldParent); err != nil {
		t.Fatal(err)
	}
	if out := putCLI(t, h, route, filepath.Join(filepath.Dir(oldParent), "next", "chat")); out.OK {
		t.Fatal("symlink parent accepted")
	}
	if _, err := os.Lstat(filepath.Join(oldParent+"-moved", filepath.Base(path))); err != nil {
		t.Fatal("original deleted", err)
	}
}

func TestCLIRejectsMountsAndChecksOwnerAndAuthority(t *testing.T) {
	h, _, route, path := cliFixture(t)
	if out := putCLI(t, h, route, "/home/bin/chat"); out.OK {
		t.Fatal("mount accepted")
	}
	raw, _ := json.Marshal(map[string]string{"server": "https://control.test", "username": "alice", "command_path": path})
	_, body := call(h, "PUT", route, string(raw), platform.User{Role: "member"})
	if !strings.Contains(body, "管理员") {
		t.Fatal(body)
	}
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err != nil {
		t.Fatal(err)
	}
	if out := putCLI(t, h, route, path); !out.OK || out.Installed != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("installed into another member container", err)
	}
	for _, dest := range []string{"/tmp/../bin/chat", "/proc/bin/chat", "relative/chat", "/tmp/a;echo", "/tmp/a\nb"} {
		if outCode, _ := call(h, "PUT", route, `{"server":"https://control.test","username":"alice","command_path":`+string(mustJSON(dest))+`}`, admin); outCode == 200 {
			t.Fatal("bad path accepted", dest)
		}
	}
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }
