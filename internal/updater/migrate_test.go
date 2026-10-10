package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"project-alpha/internal/platform"
)

type dockerRequest struct {
	Endpoint string
	Args     []string
	Input    []byte
}

type dockerResponse struct {
	Output []byte
	Error  string
}

// The built updater invokes a real child CLI. Forward only this test's Docker
// calls to its isolated fixture; never connect to the host's Docker daemon.
func TestMigrationDockerProcess(t *testing.T) {
	address := os.Getenv("ALPHA_TEST_DOCKER_RPC")
	if address == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	input, err := io.ReadAll(os.Stdin)
	if err != nil || len(args) < 3 || args[0] != "--host" {
		os.Exit(2)
	}
	raw, _ := json.Marshal(dockerRequest{args[1], args[2:], input})
	response, err := http.Post(address, "application/json", bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer response.Body.Close()
	var result dockerResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		os.Exit(2)
	}
	if result.Error != "" {
		fmt.Fprintln(os.Stderr, result.Error)
		os.Exit(1)
	}
	os.Stdout.Write(result.Output)
	os.Exit(0)
}

func useDockerProcessFixture(t *testing.T, run dockerCommand) func() {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var request dockerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		output, err := run(r.Context(), request.Endpoint, request.Args, request.Input)
		result := dockerResponse{Output: output}
		if err != nil {
			result.Error = err.Error()
		}
		json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(server.Close)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexec "+shellQuote(os.Args[0])+" -test.run=^TestMigrationDockerProcess$ -- \"$@\"\n"))
	t.Setenv("ALPHA_TEST_DOCKER_RPC", server.URL)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return func() { mu.Lock(); mu.Unlock() }
}

func migrationArchive(t *testing.T, target []byte) []byte {
	t.Helper()
	manifest, err := os.ReadFile("../../deploy/updater-services.json")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "project-alpha_v0.6.0_linux_" + runtime.GOARCH
	return makeArchive(t, map[string][]byte{
		prefix + "/bin/project-alpha":            script("project-alpha", "v0.6.0"),
		prefix + "/bin/alpha-updater":            target,
		prefix + "/bin/rootless-docker":          []byte("rootless binary"),
		prefix + "/bin/dram-bwd":                 []byte("dram binary"),
		prefix + "/deploy/updater-services.json": manifest,
	})
}

func TestMigrateValidatesInstallerContainers(t *testing.T) {
	target := buildTarget(t)
	archive := migrationArchive(t, target)
	for _, mode := range []string{"success", "current-schema", "binary-only", "current-schema-binary-only", "rollback-failure", "migration-rollback-failure", "database-failure", "unsupported", "wrong-lock", "missing-payload", "mismatched-payload"} {
		t.Run(mode, func(t *testing.T) {
			f, db, payload := newDockerFixture(t)
			syncDocker := useDockerProcessFixture(t, f.run)
			oldSchema := 37
			if strings.HasPrefix(mode, "current-schema") {
				if err := platform.UpgradeDatabase(db.Directory, "worker", nil, nil); err != nil {
					t.Fatal(err)
				}
				oldSchema = platform.DatabaseVersion
				f.container["State"] = map[string]any{"Status": "running", "Running": true}
			}
			if mode == "unsupported" {
				if _, err := db.SQL.Exec("PRAGMA user_version=36"); err != nil {
					t.Fatal(err)
				}
				oldSchema = 36
			}
			if mode == "database-failure" || mode == "migration-rollback-failure" {
				if _, err := db.SQL.Exec("CREATE TABLE mihomo_runtime(sentinel TEXT); INSERT INTO mihomo_runtime VALUES('preserved')"); err != nil {
					t.Fatal(err)
				}
			}
			binaryOnly := strings.Contains(mode, "binary-only")
			f.fail = binaryOnly || mode == "rollback-failure"
			f.rollbackFail = binaryOnly || mode == "rollback-failure" || mode == "migration-rollback-failure"
			var plan *containerPlan
			if mode == "success" || mode == "current-schema" || mode == "rollback-failure" || mode == "database-failure" || mode == "migration-rollback-failure" {
				var err error
				plan, err = prepareContainers(context.Background(), db, payload, f.run, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
			}
			bin := t.TempDir()
			stage, err := os.MkdirTemp(bin, ".alpha-stage-")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(stage, "release.tar.gz"), archive)
			var names []string
			for _, file := range binaries("worker") {
				names = append(names, file.source)
				writeFile(t, filepath.Join(bin, file.destination), []byte("old "+file.source))
			}
			// Match the binary-only caller: it extracts no DRAM binary or manifest.
			if err := extract(bytes.NewReader(archive), stage, "project-alpha_v0.6.0_linux_"+runtime.GOARCH, names); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-payload" {
				if err := os.Remove(filepath.Join(stage, "release.tar.gz")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "mismatched-payload" {
				writeFile(t, filepath.Join(stage, "rootless-docker"), []byte("different release"))
			}
			lock, err := db.LockService()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if mode == "wrong-lock" {
				lock, err = os.CreateTemp(t.TempDir(), "wrong-lock")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			migrate := func() error {
				calls := len(f.calls)
				cmd := exec.Command(filepath.Join(bin, "alpha-updater"), "_migrate", "worker", db.Directory)
				cmd.ExtraFiles = []*os.File{lock}
				raw, err := cmd.CombinedOutput()
				syncDocker()
				for _, args := range f.calls[calls:] {
					if args[0] == "build" || args[0] == "pull" || slices.Contains(args, "up") || slices.Contains(args, "start") {
						t.Fatal("migration subprocess modified containers or images", args)
					}
				}
				if err != nil {
					return fmt.Errorf("%w: %s", err, raw)
				}
				return nil
			}
			backup, err := backupDatabase(db, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			// Container rollback stays in the installer, where the recovery
			// error identity can prevent binary rollback and service restart.
			err = install(bin, stage, binaries("worker"), backup, func() error {
				if plan == nil {
					return migrate()
				}
				rollback, err := plan.apply(context.Background(), bin, f.run, io.Discard)
				if err != nil {
					return err
				}
				if err := migrate(); err != nil {
					return errors.Join(err, rollback())
				}
				return nil
			}, io.Discard)
			syncDocker()
			success := mode == "success" || mode == "current-schema"
			if success {
				if err != nil || f.container["Image"] != f.next {
					t.Fatalf("container not completed: %v, %v", err, f.container)
				}
				assertVersion(t, db, platform.DatabaseVersion)
				if f.container["State"].(map[string]any)["Running"] != (mode == "current-schema") {
					t.Fatal("service running state changed", f.container["State"])
				}
				calls := len(f.calls)
				writeFile(t, filepath.Join(bin, ".alpha-update-pending"), []byte("repeat\n"))
				if err := migrate(); err != nil {
					t.Fatal(err)
				}
				syncDocker()
				for _, args := range f.calls[calls:] {
					if slices.Contains(args, "up") || slices.Contains(args, "start") {
						t.Fatal("repeated migration rebuilt an unchanged container", args)
					}
				}
			} else if mode == "rollback-failure" || mode == "migration-rollback-failure" {
				if !errors.Is(err, errContainerUncertain) {
					t.Fatal("lost container recovery error", err)
				}
				assertVersion(t, db, oldSchema)
				journal, readErr := os.ReadFile(filepath.Join(bin, ".alpha-update-pending"))
				if readErr != nil || !strings.Contains(string(journal), "container_plan=") {
					t.Fatal("lost recovery journal", readErr, string(journal))
				}
				installed, readErr := os.ReadFile(filepath.Join(bin, "alpha-updater"))
				if readErr != nil || !bytes.Equal(installed, target) {
					t.Fatal("uncertain installation restored binaries", readErr)
				}
			} else {
				if binaryOnly && (err == nil || !strings.Contains(err.Error(), "bootstrap")) {
					t.Fatal("missing bootstrap instruction", err)
				}
				if err == nil || f.container["Image"] != f.old {
					t.Fatalf("failure not rolled back: %v, %v", err, f.container)
				}
				assertVersion(t, db, oldSchema)
				for _, file := range binaries("worker") {
					raw, err := os.ReadFile(filepath.Join(bin, file.destination))
					if err != nil || string(raw) != "old "+file.source {
						t.Fatal("caller did not restore binary", file, err)
					}
				}
				if plan == nil && !binaryOnly && len(f.calls) != 0 {
					t.Fatal("invalid migration reached Docker", f.calls)
				}
				if _, err := os.Stat(filepath.Join(bin, ".alpha-update-pending")); !os.IsNotExist(err) {
					t.Fatal("caller did not finish rollback", err)
				}
			}
			entries, _ := os.ReadDir(bin)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".alpha-migrate-") {
					t.Fatal("temporary payload leaked", entry.Name())
				}
			}
		})
	}
}

// ALPHA_TEST_RELEASE_INSTALLER can point to an updater built from a published
// tag, exercising its actual _service parent against this release's _migrate.
func TestReleaseServiceMigrationRecovery(t *testing.T) {
	const tag = "v0.8.0-rc3.2"
	const installed = "v0.8.0-rc3.1"
	target := buildTargetVersion(t, tag)
	installer := target
	if path := os.Getenv("ALPHA_TEST_RELEASE_INSTALLER"); path != "" {
		var err error
		installer, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"success", "validation-failure", "rollback-failure"} {
		t.Run(mode, func(t *testing.T) {
			f, db, payload := newDockerFixture(t)
			if err := platform.UpgradeDatabase(db.Directory, "worker", nil, nil); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "restarted")
			main := func(version string) []byte {
				return []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'project-alpha " + version + "'; else echo '" + version + "' > " + shellQuote(marker) + "; fi\n")
			}
			writeFile(t, filepath.Join(bin, "project-alpha"), main(installed))
			writeFile(t, filepath.Join(bin, "alpha-updater"), installer)
			writeFile(t, filepath.Join(bin, "rootless-docker"), script("rootless-docker", installed))
			manifest, err := os.ReadFile(filepath.Join(payload, serviceManifest))
			if err != nil {
				t.Fatal(err)
			}
			prefix := "project-alpha_" + tag + "_linux_" + runtime.GOARCH
			archive := makeArchive(t, map[string][]byte{
				prefix + "/bin/project-alpha":            main(tag),
				prefix + "/bin/alpha-updater":            target,
				prefix + "/bin/rootless-docker":          script("rootless-docker", tag),
				prefix + "/bin/dram-bwd":                 []byte("dram binary"),
				prefix + "/deploy/updater-services.json": manifest,
			})
			stage, err := os.MkdirTemp(bin, ".alpha-stage-")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(stage, "release.tar.gz"), archive)
			var names []string
			for _, file := range releaseFiles("worker") {
				names = append(names, file.source)
			}
			if err := extract(bytes.NewReader(archive), stage, prefix, names); err != nil {
				t.Fatal(err)
			}
			containers, err := prepareContainers(context.Background(), db, stage, f.run, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			prepared := &PreparedUpdate{Stage: stage, Tag: tag, Installed: installed, Schema: platform.DatabaseVersion, Hashes: map[string]string{}, Containers: containers}
			for _, file := range releaseFiles("worker") {
				hash, err := fileHash(filepath.Join(stage, file.source))
				if err != nil {
					t.Fatal(err)
				}
				prepared.Hashes[file.source] = hash
			}
			f.rollbackFail = mode == "rollback-failure"
			syncDocker := useDockerProcessFixture(t, func(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
				// The installer's fixed image IDs remain valid, but the target
				// release's image reference is missing at pre-commit validation.
				if mode != "success" && len(args) == 3 && args[0] == "image" && !strings.HasPrefix(args[2], "sha256:") {
					return nil, errors.New("injected missing release image")
				}
				return f.run(ctx, endpoint, args, input)
			})
			plan := filepath.Join(db.Directory, "update-service.json")
			if err := WriteJSON(plan, ServicePlan{Format: 3, Prepared: prepared, Directory: db.Directory, Executable: filepath.Join(bin, "project-alpha"), Command: filepath.Join(bin, "alpha-updater"), Role: "worker", Repo: "arusuki/alpha", Tag: tag, Prerelease: true, Arguments: []string{"serve", "--worker", "--data-dir", db.Directory}}); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(filepath.Join(bin, "alpha-updater"), "_service", plan).CombinedOutput()
			syncDocker()
			assertVersion(t, db, platform.DatabaseVersion)
			journal, pendingErr := os.ReadFile(filepath.Join(bin, ".alpha-update-pending"))
			restarted, restartErr := os.ReadFile(marker)
			if mode == "rollback-failure" {
				if err == nil || pendingErr != nil || !strings.Contains(string(journal), "container_plan=") || !os.IsNotExist(restartErr) {
					t.Fatalf("uncertain container recovery restarted service or lost journal: %v, %v, %v\n%s", err, pendingErr, restartErr, output)
				}
				got, readErr := os.ReadFile(filepath.Join(bin, "alpha-updater"))
				if readErr != nil || !bytes.Equal(got, target) {
					t.Fatal("restored binaries despite failed container rollback", readErr)
				}
			} else {
				want := tag
				if mode == "validation-failure" {
					want = installed
				}
				if err != nil || !os.IsNotExist(pendingErr) || restartErr != nil || strings.TrimSpace(string(restarted)) != want {
					t.Fatalf("unexpected service restart: %v, %v, %s\n%s", err, pendingErr, restarted, output)
				}
			}
			var result ServiceResult
			raw, err := os.ReadFile(filepath.Join(db.Directory, "update-result.json"))
			if err != nil || json.Unmarshal(raw, &result) != nil {
				t.Fatal("missing update result", err)
			}
			if mode == "success" {
				if result.State != "completed" || f.container["Image"] != f.next {
					t.Fatal(result, f.container)
				}
			} else if result.State != "failed" || !strings.Contains(result.Error, "bootstrap") {
				t.Fatal("missing actionable failure", result)
			}
		})
	}
}
