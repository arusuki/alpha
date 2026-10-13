package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"project-alpha/internal/platform"
)

func TestExternalUpdaterAcrossDatabaseReleases(t *testing.T) {
	db := oldDatabase(t, "registry")
	bin := t.TempDir()
	command := filepath.Join(t.TempDir(), "custom-updater")
	writeFile(t, command, script("alpha-updater", "v0.5.4"))
	writeFile(t, filepath.Join(bin, "project-alpha"), script("project-alpha", "v0.5.4"))
	for i, tag := range []string{"v0.6.0", "v0.7.0"} {
		schema := platform.DatabaseVersion + i + 1
		target := buildFutureUpdater(t, 0, schema, tag)
		g := archiveServerVersion(t, futureArchiveVersion(t, target, tag), false, tag)
		o := options{role: "registry", directory: db.Directory, binDir: bin, command: command, bootstrap: true}
		// This bootstrap remains older than both databases; only the target
		// interprets the migration window, including a repeated no-op update.
		for repeat := 0; repeat < 2; repeat++ {
			if err := run(context.Background(), o, g, io.Discard); err != nil {
				t.Fatal(tag, repeat, err)
			}
			assertVersion(t, db, schema)
		}
		for _, path := range []string{command, filepath.Join(bin, "alpha-updater")} {
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, target) {
				t.Fatal("updater not replaced", path, err)
			}
		}
		// Invoke the configured executable, not the copy in binDir.
		if raw, err := exec.Command(command, "--database-only", "--data-dir", db.Directory).CombinedOutput(); err != nil {
			t.Fatalf("configured updater cannot read upgraded database: %v %s", err, raw)
		}
		assertNoStaging(t, bin)
	}
}

func TestExternalUpdaterServiceHandoff(t *testing.T) {
	source := buildTarget(t)
	target := futureUpdater(t, 0)
	archive := futureArchive(t, target)
	for _, mode := range []string{"success", "migration-failure", "container-rollback-failure", "changed-updater", "unwritable"} {
		t.Run(mode, func(t *testing.T) {
			f, db, _ := newDockerFixture(t)
			f.fail, f.rollbackFail = mode == "container-rollback-failure", mode == "container-rollback-failure"
			image := func() string {
				f.mu.Lock()
				defer f.mu.Unlock()
				return f.container["Image"].(string)
			}
			syncDocker := useDockerProcessFixture(t, func(ctx context.Context, ep string, args []string, input []byte) ([]byte, error) {
				if args[0] == "build" {
					return nil, nil
				}
				return f.run(ctx, ep, args, input)
			})
			if mode == "migration-failure" {
				if _, err := db.SQL.Exec("CREATE TRIGGER reject_upgrade BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			bin, external := t.TempDir(), t.TempDir()
			command := filepath.Join(external, "custom-updater")
			writeFile(t, command, source)
			marker := filepath.Join(bin, "restarted")
			t.Setenv("ALPHA_TEST_RESTART", marker)
			main := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'project-alpha v0.5.4'; else echo restarted > \"$ALPHA_TEST_RESTART\"; fi\n")
			writeFile(t, filepath.Join(bin, "project-alpha"), main)
			writeFile(t, filepath.Join(bin, "alpha-updater"), script("alpha-updater", "v0.5.4"))
			writeFile(t, filepath.Join(bin, "rootless-docker"), script("rootless-docker", "v0.5.4"))
			p := ServicePlan{Format: 3, Command: command, Executable: filepath.Join(bin, "project-alpha"), Directory: db.Directory, Role: "worker", Repo: "arusuki/alpha", Arguments: []string{"--worker"}}
			path := filepath.Join(db.Directory, "update-service.json")
			if err := WriteJSON(path, p); err != nil {
				t.Fatal(err)
			}
			lock, err := db.LockService()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if mode == "unwritable" {
				if os.Geteuid() == 0 {
					t.Skip("permission denial requires an ordinary service account")
				}
				if err := os.Chmod(external, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(external, 0700)
			}
			o := p.options()
			o.bootstrap, o.servicePlan = true, path
			err = run(context.Background(), o, archiveServer(t, archive, false), io.Discard)
			syncDocker()
			assertVersion(t, db, 41)
			if image() != f.old {
				t.Fatal("preparation replaced running deployment")
			}
			if mode == "unwritable" {
				if err == nil || !strings.Contains(err.Error(), "not writable") {
					t.Fatal("unwritable updater did not fail preparation", err)
				}
				assertNoStaging(t, bin)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			original := source
			if mode == "changed-updater" {
				original = append(bytes.Clone(source), []byte("changed since preparation")...)
				writeFile(t, command, original)
			}
			lock.Close()
			// Exercise both exec handoffs with actual updater executables.
			raw, err := exec.Command(command, "_service", path).CombinedOutput()
			syncDocker()
			uncertain := mode == "container-rollback-failure"
			if uncertain != (err != nil) {
				t.Fatalf("handoff %v: %s", err, raw)
			}
			_, restartErr := os.Stat(marker)
			if uncertain != os.IsNotExist(restartErr) {
				t.Fatal("wrong restart outcome", restartErr)
			}
			raw, err = os.ReadFile(filepath.Join(db.Directory, "update-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result ServiceResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			wantUpdater, wantSchema := original, 41
			if mode == "success" {
				wantUpdater, wantSchema = target, platform.DatabaseVersion
				if result.State != "completed" || image() != f.next {
					t.Fatal(result)
				}
			} else if result.State != "failed" {
				t.Fatal(result)
			}
			if uncertain {
				wantUpdater = target
			}
			assertVersion(t, db, wantSchema)
			raw, err = os.ReadFile(command)
			if err != nil || !bytes.Equal(raw, wantUpdater) {
				t.Fatal("configured updater has wrong recovery state", err)
			}
			_, pendingErr := os.Stat(filepath.Join(bin, ".alpha-update-pending"))
			if uncertain == os.IsNotExist(pendingErr) {
				t.Fatal("wrong recovery marker state", pendingErr)
			}
		})
	}
}

func TestConfiguredUpdaterDestinationsAndRollback(t *testing.T) {
	bin := t.TempDir()
	installed := filepath.Join(bin, "alpha-updater")
	writeFile(t, installed, []byte("new updater"))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(bin, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{installed, filepath.Join(alias, "alpha-updater")} {
		if p, err := prepareConfiguredUpdater(path, bin, "control"); err != nil || p != nil {
			t.Fatal("same destination must be installed only once", p, err)
		}
	}
	if _, err := prepareConfiguredUpdater(filepath.Join(bin, "project-alpha"), bin, "control"); err == nil {
		t.Fatal("accepted main executable as updater destination")
	}
	hardlink := filepath.Join(t.TempDir(), "hardlink")
	if err := os.Link(installed, hardlink); err != nil {
		t.Fatal(err)
	}
	if p, err := prepareConfiguredUpdater(hardlink, bin, "control"); err != nil || p == nil {
		t.Fatal("a separate hard link must also be replaced", p, err)
	}
	if _, err := prepareConfiguredUpdater(alias, bin, "control"); err == nil {
		t.Fatal("accepted a symlink as updater")
	}
	for _, failRollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "rollback-failure"}[failRollback], func(t *testing.T) {
			command := filepath.Join(t.TempDir(), "custom-updater")
			writeFile(t, command, []byte("old updater"))
			p, err := prepareConfiguredUpdater(command, bin, "control")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(bin, ".alpha-update-pending"), []byte("test\n"))
			rollback, err := p.apply(bin, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(command)
			if err != nil || string(raw) != "new updater" {
				t.Fatal("updater not replaced", err)
			}
			if failRollback {
				if err := os.Remove(command); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(command, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = rollback()
			if failRollback {
				if !errors.Is(err, errUpdaterUncertain) {
					t.Fatal("lost recovery error", err)
				}
			} else {
				raw, readErr := os.ReadFile(command)
				if err != nil || readErr != nil || string(raw) != "old updater" {
					t.Fatal("updater not restored", err, readErr)
				}
			}
			journal, err := os.ReadFile(filepath.Join(bin, ".alpha-update-pending"))
			if err != nil || !strings.Contains(string(journal), "configured_updater_backup=") {
				t.Fatal("missing updater recovery record", err)
			}
		})
	}
}

func TestConfiguredUpdaterAcrossFilesystems(t *testing.T) {
	external, err := os.MkdirTemp("/dev/shm", "alpha-updater-test-")
	if err != nil {
		t.Skip("second writable filesystem unavailable:", err)
	}
	defer os.RemoveAll(external)
	bin := t.TempDir()
	a, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(external)
	if err != nil {
		t.Fatal(err)
	}
	if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
		t.Skip("temporary directories share a filesystem")
	}
	command := filepath.Join(external, "custom-updater")
	writeFile(t, command, []byte("old updater"))
	writeFile(t, filepath.Join(bin, "alpha-updater"), []byte("new updater"))
	writeFile(t, filepath.Join(bin, ".alpha-update-pending"), []byte("test\n"))
	p, err := prepareConfiguredUpdater(command, bin, "registry")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := p.lock(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if other, err := p.lock(bin); err == nil {
		other()
		t.Fatal("shared updater was not locked")
	}
	rollback, err := p.apply(bin, io.Discard)
	if err != nil {
		t.Fatal("cross-filesystem installation failed", err)
	}
	if raw, err := os.ReadFile(command); err != nil || string(raw) != "new updater" {
		t.Fatal("configured updater not installed", err)
	}
	if err := rollback(); err != nil {
		t.Fatal("cross-filesystem rollback failed", err)
	}
	if raw, err := os.ReadFile(command); err != nil || string(raw) != "old updater" {
		t.Fatal("configured updater not restored", err)
	}
}

func TestBootstrapSameVersionRejectsUnsupportedDatabase(t *testing.T) {
	db := oldDatabase(t, "registry")
	if _, err := db.SQL.Exec("PRAGMA user_version=1000"); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "project-alpha"), script("project-alpha", "v0.6.0"))
	err := run(context.Background(), options{role: "registry", directory: db.Directory, binDir: bin, bootstrap: true}, releaseServer(t, buildTarget(t), false), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported database version 1000") {
		t.Fatal("no-op bypassed target database validation", err)
	}
	assertVersion(t, db, 1000)
	assertNoStaging(t, bin)
}
