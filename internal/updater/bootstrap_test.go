package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

// Build a target that this test process cannot interpret: both its service
// manifest and prepared deployment fields differ, and it requires a new file.
// The overlay affects only the child build, never the working tree.
func futureUpdater(t *testing.T, minimum int) []byte {
	return buildFutureUpdater(t, minimum, platform.DatabaseVersion, "v0.6.0")
}

func buildFutureUpdater(t *testing.T, minimum, schema int, tag string) []byte {
	t.Helper()
	dir := t.TempDir()
	replacements := map[string]string{}
	for _, name := range []string{"containers.go", "updater.go"} {
		path, err := filepath.Abs(name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if name == "containers.go" {
			text = strings.ReplaceAll(text, "Binaries", "FutureInputs")
			text = strings.Replace(text, "Endpoint string\n\tUpdates  []containerUpdate", "Endpoint string `json:\"future_endpoint\"`\n\tUpdates []containerUpdate `json:\"future_updates\"`", 1)
		} else {
			text = strings.Replace(text, `binary{"dram-bwd", ""}`, `binary{"dram-bwd", ""}, binary{"future-helper", ""}`, 1)
		}
		if text == string(raw) {
			t.Fatal("overlay did not change", name)
		}
		copy := filepath.Join(dir, name)
		if err := os.WriteFile(copy, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		replacements[path] = copy
	}
	if schema > platform.DatabaseVersion {
		for _, name := range []string{"database.go", "upgrade_history.go"} {
			path, err := filepath.Abs(filepath.Join("../platform", name))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source := string(raw)
			if name == "database.go" {
				source = strings.Replace(source, fmt.Sprintf("const DatabaseVersion = %d", platform.DatabaseVersion), fmt.Sprintf("const DatabaseVersion = %d", schema), 1)
			} else {
				var steps strings.Builder
				steps.WriteString("\nfunc init() {\n")
				for v := platform.DatabaseVersion; v < schema; v++ {
					fmt.Fprintf(&steps, "databaseUpgrades = append(databaseUpgrades, databaseUpgrade{From:%d, To:%d, Apply:func(*sql.Tx)error{return nil}})\n", v, v+1)
				}
				steps.WriteString("}\n")
				source += steps.String()
			}
			copy := filepath.Join(dir, name)
			if err := os.WriteFile(copy, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			replacements[path] = copy
		}
	}
	if minimum != 0 {
		path, err := filepath.Abs("../platform/upgrade_history.json")
		if err != nil {
			t.Fatal(err)
		}
		copy := filepath.Join(dir, "upgrade_history.json")
		if err := WriteJSON(copy, map[string]any{"base_schema": minimum, "updates": []any{}}); err != nil {
			t.Fatal(err)
		}
		replacements[path] = copy
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err := WriteJSON(overlay, map[string]any{"Replace": replacements}); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "alpha-updater")
	cmd := exec.Command("go", "build", "-overlay", overlay, "-ldflags=-X project-alpha/internal/buildinfo.Version="+tag, "-o", binary, "../../cmd/alpha-updater")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build future updater: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func futureArchive(t *testing.T, target []byte) []byte {
	return futureArchiveVersion(t, target, "v0.6.0")
}

func futureArchiveVersion(t *testing.T, target []byte, tag string) []byte {
	t.Helper()
	manifest, err := os.ReadFile("../../deploy/updater-services.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.ReplaceAll(manifest, []byte(`"binaries"`), []byte(`"FutureInputs"`))
	prefix := "project-alpha_" + tag + "_linux_" + runtime.GOARCH
	main := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'project-alpha " + tag + "'; else printf '%s\\n' \"$@\" > \"$ALPHA_TEST_RESTART\"; fi\n")
	return makeArchive(t, map[string][]byte{
		prefix + "/bin/alpha-updater":            target,
		prefix + "/bin/project-alpha":            main,
		prefix + "/bin/alpha-chat":               script("alpha-chat", tag),
		prefix + "/bin/rootless-docker":          script("rootless-docker", "v0.6.0"),
		prefix + "/bin/dram-bwd":                 script("dram-bwd", "v0.6.0"),
		prefix + "/bin/future-helper":            []byte("new release payload"),
		prefix + "/deploy/updater-services.json": manifest,
	})
}

func assertNoStaging(t *testing.T, bin string) {
	t.Helper()
	for _, prefix := range []string{".alpha-bootstrap-*", ".alpha-stage-*", ".alpha-migrate-*"} {
		paths, err := filepath.Glob(filepath.Join(bin, prefix))
		if err != nil || len(paths) != 0 {
			t.Fatalf("staging leaked: %v %v", paths, err)
		}
	}
}

func TestBootstrapTargetOwnsPreparationAndInstallation(t *testing.T) {
	target := futureUpdater(t, 0)
	archive := futureArchive(t, target)
	useDockerProcessFixture(t, noDeployedServices)
	for _, role := range []string{"control", "registry", "worker", "share-node"} {
		t.Run("cli/"+role, func(t *testing.T) {
			bin := t.TempDir()
			o := options{role: role, directory: t.TempDir(), binDir: bin, repo: "arusuki/alpha", bootstrap: true}
			var db *platform.Database
			if role != "share-node" {
				db = oldDatabase(t, role)
				o.directory = db.Directory
			}
			for _, file := range binaries(role) {
				writeFile(t, filepath.Join(bin, file.destination), script(file.source, "v0.5.4"))
			}
			if err := run(context.Background(), o, archiveServer(t, archive, false), io.Discard); err != nil {
				t.Fatal(err)
			}
			if db != nil {
				assertVersion(t, db, platform.DatabaseVersion)
			}
			got, _ := os.ReadFile(filepath.Join(bin, "alpha-updater"))
			if !bytes.Equal(got, target) {
				t.Fatal("target updater not installed")
			}
			assertNoStaging(t, bin)
		})
	}
	for _, mode := range []string{"success", "tampered-updater", "tampered-archive", "migration-failure"} {
		t.Run("service/"+mode, func(t *testing.T) {
			db := oldDatabase(t, "worker")
			if mode == "migration-failure" {
				if _, err := db.SQL.Exec("CREATE TRIGGER reject_upgrade BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "restarted")
			t.Setenv("ALPHA_TEST_RESTART", marker)
			exe := filepath.Join(bin, "project-alpha")
			original := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'project-alpha v0.5.4'; else printf '%s\\n' \"$@\" > \"$ALPHA_TEST_RESTART\"; fi\n")
			writeFile(t, exe, original)
			writeFile(t, filepath.Join(bin, "alpha-updater"), script("alpha-updater", "v0.5.4"))
			writeFile(t, filepath.Join(bin, "rootless-docker"), script("rootless-docker", "v0.5.4"))
			path := filepath.Join(db.Directory, "update-service.json")
			p := ServicePlan{Format: 3, Command: filepath.Join(bin, "alpha-updater"), Executable: exe, Directory: db.Directory, Role: "worker", Repo: "arusuki/alpha", Arguments: []string{"--worker", "--data-dir", db.Directory}}
			if err := WriteJSON(path, p); err != nil {
				t.Fatal(err)
			}
			lock, err := db.LockService()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			o := p.options()
			o.bootstrap, o.servicePlan = true, path
			o.prepare = func(*PreparedUpdate) error { t.Fatal("bootstrap prepared release itself"); return nil }
			if err := run(context.Background(), o, archiveServer(t, archive, false), io.Discard); err != nil {
				t.Fatal(err)
			}
			prepared, err := ReadServicePlan(path)
			if err != nil || prepared.Prepared == nil {
				t.Fatal("missing preparation", err)
			}
			stage := prepared.Prepared.Stage
			defer os.RemoveAll(stage)
			if !bytes.Contains(prepared.Prepared.Containers, []byte("future_updates")) {
				t.Fatal("target did not own plan encoding")
			}
			if _, err := os.Stat(filepath.Join(stage, "future-helper")); err != nil {
				t.Fatal("target did not extract new payload", err)
			}
			if _, err := prepareContainers(context.Background(), db, stage, noDeployedServices, io.Discard); err == nil || !strings.Contains(err.Error(), "FutureInputs") {
				t.Fatal("fixture must be incompatible with bootstrap", err)
			}
			assertVersion(t, db, 41)
			got, _ := os.ReadFile(exe)
			if !bytes.Equal(got, original) {
				t.Fatal("online preparation replaced service")
			}
			if mode == "tampered-updater" {
				writeFile(t, filepath.Join(stage, "alpha-updater"), script("alpha-updater", "v0.6.0"))
			}
			if mode == "tampered-archive" {
				writeFile(t, filepath.Join(stage, "release.tar.gz"), []byte("changed archive"))
			}
			// The installer must work without GitHub or a download proxy.
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			lock.Close()
			child := exec.Command(os.Args[0], "-test.run=^TestServiceUpdateProcess$")
			child.Env = append(os.Environ(), "ALPHA_TEST_SERVICE_PLAN="+path)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("handoff: %v\n%s", err, output)
			}
			args, err := os.ReadFile(marker)
			if err != nil || string(args) != "--worker\n--data-dir\n"+db.Directory+"\n" {
				t.Fatalf("restart lost arguments: %s %v", args, err)
			}
			var result ServiceResult
			raw, _ := os.ReadFile(filepath.Join(db.Directory, "update-result.json"))
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if mode == "success" {
				assertVersion(t, db, platform.DatabaseVersion)
				if result.State != "completed" {
					t.Fatal(result.Error)
				}
				assertNoStaging(t, bin)
			} else {
				assertVersion(t, db, 41)
				got, _ = os.ReadFile(exe)
				if !bytes.Equal(got, original) || result.State != "failed" {
					t.Fatal("failed install did not preserve old service", result)
				}
				assertNoStaging(t, bin)
			}
			if _, err := os.Stat(filepath.Join(bin, ".alpha-update-pending")); !os.IsNotExist(err) {
				t.Fatal("safe outcome left recovery marker", err)
			}
		})
	}
}

func TestBootstrapTargetRejectsExpiredDatabase(t *testing.T) {
	// The bootstrap supports this database; the target's newer window does not.
	target := futureUpdater(t, 42)
	db := oldDatabase(t, "worker")
	bin := t.TempDir()
	for _, file := range binaries("worker") {
		writeFile(t, filepath.Join(bin, file.destination), script(file.source, "v0.5.4"))
	}
	path := filepath.Join(db.Directory, "update-service.json")
	p := ServicePlan{Format: 3, Command: filepath.Join(bin, "alpha-updater"), Executable: filepath.Join(bin, "project-alpha"), Directory: db.Directory, Role: "worker", Repo: "arusuki/alpha"}
	if err := WriteJSON(path, p); err != nil {
		t.Fatal(err)
	}
	lock, err := db.LockService()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	o := p.options()
	o.bootstrap, o.servicePlan = true, path
	o.prepare = func(*PreparedUpdate) error { return nil }
	err = run(context.Background(), o, archiveServer(t, futureArchive(t, target), false), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported database version 41") || !strings.Contains(err.Error(), "existing data preserved") {
		t.Fatal("target accepted expired database", err)
	}
	assertVersion(t, db, 41)
	for _, file := range binaries("worker") {
		got, _ := os.ReadFile(filepath.Join(bin, file.destination))
		if !bytes.Equal(got, script(file.source, "v0.5.4")) {
			t.Fatal("target rejection replaced binary", file)
		}
	}
	plan, err := ReadServicePlan(path)
	if err != nil || plan.Prepared != nil {
		t.Fatal("target rejection left prepared plan", err)
	}
	assertNoStaging(t, bin)
}

func TestBootstrapRejectsUnverifiedOrUnsupportedUpdater(t *testing.T) {
	for _, mode := range []string{"checksum", "version", "protocol", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			db := oldDatabase(t, "registry")
			bin := t.TempDir()
			original := script("project-alpha", "v0.5.4")
			writeFile(t, filepath.Join(bin, "project-alpha"), original)
			marker := filepath.Join(bin, "executed")
			version := "v0.6.0"
			if mode == "version" {
				version = "v0.5.9"
			}
			target := []byte(fmt.Sprintf("#!/bin/sh\ntouch %s\ncase \"$1\" in\n--version) echo 'alpha-updater %s';;\n--bootstrap-protocol) echo %s;;\n_release) exec sleep 30;;\nesac\n", shellQuote(marker), version, map[bool]string{true: "1", false: "99"}[mode == "cancel"]))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				timer := time.AfterFunc(500*time.Millisecond, cancel)
				defer timer.Stop()
			}
			err := run(ctx, options{role: "registry", directory: db.Directory, binDir: bin, bootstrap: true}, releaseServer(t, target, mode == "checksum"), io.Discard)
			if err == nil {
				t.Fatal("invalid target accepted")
			}
			if mode == "checksum" {
				if _, e := os.Stat(marker); !os.IsNotExist(e) {
					t.Fatal("unverified updater executed", e)
				}
			}
			got, _ := os.ReadFile(filepath.Join(bin, "project-alpha"))
			if !bytes.Equal(got, original) {
				t.Fatal("failed bootstrap changed installed binary")
			}
			assertVersion(t, db, 41)
			assertNoStaging(t, bin)
		})
	}
}
