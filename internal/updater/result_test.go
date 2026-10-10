package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readResult(t *testing.T, directory string) ServiceResult {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "update-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result ServiceResult
	if err := json.Unmarshal(raw, &result); err != nil || !result.Valid() {
		t.Fatalf("invalid result %s: %v", raw, err)
	}
	return result
}

func TestRecoveryCommandQuotesArgumentsAndOmitsSecrets(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "updater's $(false)")
	writeFile(t, command, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"))
	p := ServicePlan{Command: command, Directory: filepath.Join(dir, "data ;\n$(false)"), Executable: filepath.Join(dir, "bin's", "project-alpha"), Role: "worker", Repo: "arusuki/alpha", Prerelease: true, ServicesOnly: true, Proxy: "http://user:private-password@localhost:7890", Prepared: &PreparedUpdate{Tag: "v0.8.0-rc2"}}
	t.Setenv("GH_TOKEN", "private-github-token")
	result := p.Result(fmt.Errorf("request failed: %s %s", p.Proxy, os.Getenv("GH_TOKEN")))
	raw, _ := json.Marshal(result)
	for _, secret := range []string{"private-password", "private-github-token"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("credential leaked: %s", raw)
		}
	}
	output, err := exec.Command("sh", "-c", result.Recovery.Command).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--role", p.Role, "--data-dir", p.Directory, "--bin-dir", filepath.Dir(p.Executable), "--repo", p.Repo, "--tag", p.Prepared.Tag, "--prerelease", "--services-only"}
	if got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"); !reflect.DeepEqual(got, want) {
		t.Fatalf("retry arguments: %q, want %q", got, want)
	}
	if !strings.Contains(result.Recovery.Note, "HTTPS_PROXY") {
		t.Fatal("missing proxy environment instructions")
	}
}

func TestCLIReportsFailureThenSuccessfulRetry(t *testing.T) {
	o := options{directory: t.TempDir(), binDir: t.TempDir(), role: "worker", repo: "arusuki/alpha", tag: "v0.8.0-rc2", servicesOnly: true}
	failure := errors.New("Docker socket permission denied")
	err := reportCLI(o, io.Discard, func(out io.Writer) error {
		fmt.Fprintln(out, "checking installed containers")
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	result := readResult(t, o.directory)
	if result.State != "failed" || result.Error != failure.Error() || result.Recovery == nil || !strings.Contains(result.Recovery.Command, "--services-only") {
		t.Fatalf("missing retry report: %+v", result)
	}
	log, err := os.ReadFile(result.Recovery.LogPath)
	if err != nil || !bytes.Contains(log, []byte(failure.Error())) || !bytes.Contains(log, []byte("checking installed containers")) {
		t.Fatalf("missing log: %s %v", log, err)
	}
	for _, file := range []string{"update-result.json", "update.log"} {
		info, err := os.Stat(filepath.Join(o.directory, file))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("report permissions", err)
		}
	}
	for _, mode := range []string{"check", "database-only"} {
		check := o
		check.check, check.databaseOnly = mode == "check", mode == "database-only"
		if err := reportCLI(check, io.Discard, func(io.Writer) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if readResult(t, o.directory).State != "failed" {
			t.Fatalf("%s erased unresolved failure", mode)
		}
	}
	if err := reportCLI(o, io.Discard, func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	result = readResult(t, o.directory)
	if result.State != "completed" || result.Error != "" || result.Recovery != nil {
		t.Fatalf("stale failure: %+v", result)
	}
}

func TestBuiltUpdaterPublishesCLIError(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	command := filepath.Join(bin, "alpha-updater")
	writeFile(t, command, buildTarget(t))
	output, err := exec.Command(command, "--data-dir", dir, "--bin-dir", bin, "--http-proxy", "invalid").CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("invalid --http-proxy")) {
		t.Fatalf("expected CLI failure: %s %v", output, err)
	}
	result := readResult(t, dir)
	if result.State != "failed" || result.Recovery == nil || !strings.HasPrefix(result.Recovery.Command, shellQuote(command)) {
		t.Fatalf("CLI entrypoint did not report: %+v", result)
	}
}

func TestServiceStartupFailuresAreReported(t *testing.T) {
	for _, stage := range []string{"handoff", "plan", "log", "restart"} {
		t.Run(stage, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			path := filepath.Join(dir, "update-service.json")
			p := ServicePlan{Format: 3, Command: filepath.Join(bin, "alpha-updater"), Directory: dir, Executable: filepath.Join(bin, "project-alpha"), Role: "worker", Repo: "arusuki/alpha"}
			if err := WriteJSON(path, p); err != nil {
				t.Fatal(err)
			}
			if stage == "plan" {
				writeFile(t, path, []byte("invalid"))
			}
			if stage == "log" {
				if err := os.Mkdir(filepath.Join(dir, "update.log"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if stage == "handoff" {
				err = (&Handoff{Command: p.Command, PlanPath: path}).Exec()
			} else {
				err = service(context.Background(), path, io.Discard)
			}
			if err == nil || readResult(t, dir).State != "failed" {
				t.Fatal("unreported startup failure", err)
			}
			if stage == "restart" {
				detail := readResult(t, dir).Error
				if !strings.Contains(detail, "no prepared release") || !strings.Contains(detail, "启动主程序失败") {
					t.Fatal("lost original failure after restart error", detail)
				}
			}
		})
	}
}
