package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

func agnoArchiveEntries(t *testing.T, prefix string, entries map[string][]byte) map[string][]byte {
	t.Helper()
	for _, name := range append([]string{agnoDockerfile}, agnoBuildFiles...) {
		raw, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		entries[prefix+"/"+name] = raw
	}
	return entries
}

func newAgnoFixture(t *testing.T) (*dockerFixture, *platform.Database, string) {
	t.Helper()
	f, _, stage := newDockerFixture(t)
	db := oldDatabase(t, "control")
	f.service, f.endpoint = "agno", "unix:///test/docker.sock"
	t.Setenv("DOCKER_HOST", f.endpoint)
	t.Setenv("ALPHA_AGNO_SOCKET", "")
	for name, raw := range agnoArchiveEntries(t, "", map[string][]byte{}) {
		file := filepath.Join(stage, strings.TrimPrefix(name, "/"))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, file, raw)
	}
	config := f.container["Config"].(map[string]any)
	labels := config["Labels"].(map[string]string)
	labels["project-alpha.service"], labels["com.docker.compose.service"] = "agno", "agno"
	config["User"] = "1005:1006"
	config["Entrypoint"] = []string{"python", "-I", "-u", "/app/service.py"}
	config["Cmd"] = nil
	f.container["Mounts"] = []any{map[string]any{"Type": "bind", "Source": filepath.Join(db.Directory, "agno"), "Destination": "/run/agno"}}
	f.compose["services"] = map[string]any{"agno": map[string]any{
		"image": "old", "user": "1005:1006", "build": map[string]any{"context": "/old/source"},
		"network_mode": "host", "read_only": true, "init": true, "cap_drop": []string{"ALL"},
		"security_opt": []string{"no-new-privileges:true"}, "mem_limit": "1g", "pids_limit": 64,
		"labels":  map[string]any{"project-alpha.service": "agno"},
		"volumes": []any{map[string]any{"type": "bind", "source": filepath.Join(db.Directory, "agno"), "target": "/run/agno", "bind": map[string]any{"create_host_path": false}}},
	}}
	return f, db, stage
}

func TestAgnoPreparePreservesDeploymentAndHashesSources(t *testing.T) {
	f, db, stage := newAgnoFixture(t)
	var refs []string
	run := func(ctx context.Context, ep string, args []string, input []byte) ([]byte, error) {
		if args[0] == "build" {
			if slices.Contains(args, "--network=none") {
				t.Fatal("Agno dependencies need build network")
			}
			refs = append(refs, args[slices.Index(args, "--tag")+1])
			files := map[string][]byte{}
			contextDir := args[len(args)-1]
			if len(input) != 0 {
				t.Fatal("Agno build must use an isolated local context")
			}
			for _, name := range append([]string{agnoDockerfile}, agnoBuildFiles...) {
				raw, err := os.ReadFile(filepath.Join(contextDir, name))
				if err != nil {
					t.Fatal(err)
				}
				files[name] = raw
			}

			if !bytes.Contains(files[agnoDockerfile], []byte("FROM python:")) {
				t.Fatal("missing Dockerfile")
			}
			for _, name := range agnoBuildFiles {
				if len(files[name]) == 0 {
					t.Fatal("missing source", name)
				}
			}
		}
		if args[0] == "compose" && slices.Contains(args, "config") {
			index := -1
			for i, arg := range args {
				if arg == "--env-file" {
					index = i
				}
			}
			if index < 0 {
				t.Fatal("missing recovered deployment environment")
			}
			raw, err := os.ReadFile(args[index+1])
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"AGNO_UID=1005", "AGNO_GID=1006", filepath.Join(db.Directory, "agno")} {
				if !bytes.Contains(raw, []byte(want)) {
					t.Fatal("lost deployment identity", want)
				}
			}
		}
		return f.run(ctx, ep, args, input)
	}
	p, err := prepareContainers(context.Background(), db, stage, run, io.Discard)
	if err != nil || len(p.Updates) != 1 {
		t.Fatal(p, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(p.Updates[0].After, &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc["services"].(map[string]any)["agno"].(map[string]any)
	if spec["image"] != f.next || spec["user"] != "1005:1006" || spec["network_mode"] != "host" || spec["read_only"] != true || spec["volumes"] == nil || spec["cap_drop"] == nil || spec["build"] != nil {
		t.Fatal(spec)
	}
	for _, args := range f.recordedCalls() {
		if slices.Contains(args, "up") || slices.Contains(args, "start") {
			t.Fatal("preparation modified running deployment")
		}
	}
	file := filepath.Join(stage, "runtime/agno/runtime.py")
	before, _ := os.ReadFile(file)
	writeFile(t, file, append(before, []byte("\n# new release\n")...))
	if _, err := prepareContainers(context.Background(), db, stage, run, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] == refs[1] {
		t.Fatal("runtime source changes did not change image tag", refs)
	}
}

func TestAgnoPreparedControlInstallAndRollback(t *testing.T) {
	target := buildTarget(t)
	for _, mode := range []string{"success", "build-failure", "recreate-failure", "migration-failure", "tampered-source", "deployment-drift", "wrong-socket", "compose-mount-drift"} {
		t.Run(mode, func(t *testing.T) {
			f, db, _ := newAgnoFixture(t)
			syncDocker := useDockerProcessFixture(t, f.run)
			f.buildFail = mode == "build-failure"
			if mode == "wrong-socket" {
				t.Setenv("ALPHA_AGNO_SOCKET", "/another-control/agno.sock")
			}
			if mode == "compose-mount-drift" {
				spec := f.compose["services"].(map[string]any)["agno"].(map[string]any)
				spec["volumes"].([]any)[0].(map[string]any)["source"] = "/different/socket"
			}
			bin := t.TempDir()
			oldMain := script("project-alpha", "v0.5.4")
			writeFile(t, filepath.Join(bin, "project-alpha"), oldMain)
			writeFile(t, filepath.Join(bin, "alpha-updater"), script("alpha-updater", "v0.5.4"))
			if mode == "migration-failure" {
				if _, err := db.SQL.Exec(`CREATE TRIGGER fail_agno BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var prepared *PreparedUpdate
			o := options{role: "control", directory: db.Directory, binDir: bin, docker: f.run, prepare: func(p *PreparedUpdate) error { prepared = p; return nil }}
			lock, err := db.LockService()
			if err != nil {
				t.Fatal(err)
			}
			err = run(context.Background(), o, releaseServer(t, target, false), io.Discard)
			lock.Close()
			if mode == "build-failure" || mode == "wrong-socket" || mode == "compose-mount-drift" {
				if err == nil || prepared != nil {
					t.Fatal("unsafe preparation accepted", err)
				}
				assertVersion(t, db, 41)
				return
			}
			if err != nil || prepared == nil {
				t.Fatal(err)
			}
			assertVersion(t, db, 41)
			calls := len(f.recordedCalls())
			if mode == "tampered-source" {
				writeFile(t, filepath.Join(prepared.Stage, "runtime/agno/runtime.py"), []byte("tampered"))
			}
			if mode == "deployment-drift" {
				f.container["Config"].(map[string]any)["User"] = "1234:1234"
			}
			f.fail = mode == "recreate-failure"
			o.prepare, o.prepared = nil, prepared
			err = runConfigured(context.Background(), o, io.Discard)
			syncDocker()
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
				assertVersion(t, db, platform.DatabaseVersion)
				if f.container["Image"] != f.next {
					t.Fatal("Agno not updated")
				}
			} else {
				if err == nil {
					t.Fatal("expected failed update", mode)
				}
				assertVersion(t, db, 41)
				actual, _ := os.ReadFile(filepath.Join(bin, "project-alpha"))
				if !bytes.Equal(actual, oldMain) || f.container["Image"] != f.old {
					t.Fatal("failed update did not preserve binaries and Agno", err)
				}
			}
			for _, args := range f.recordedCalls()[calls:] {
				if args[0] == "build" || args[0] == "pull" {
					t.Fatal("offline install accessed image build/network", args)
				}
			}
			if mode == "success" {
				o.prepared = nil
				o.servicesOnly = true
				if err := run(context.Background(), o, releaseServer(t, target, false), io.Discard); err != nil {
					t.Fatal("repeat service sync", err)
				}
				if f.container["State"].(map[string]any)["Running"] != false {
					t.Fatal("stopped Agno was started")
				}
			}
		})
	}
}

func TestControlReleaseRequiresAgnoPayload(t *testing.T) {
	for _, missing := range append([]string{agnoDockerfile}, agnoBuildFiles...) {
		t.Run(missing, func(t *testing.T) {
			prefix := "package"
			entries := agnoArchiveEntries(t, prefix, map[string][]byte{})
			delete(entries, prefix+"/"+missing)
			names := append([]string{agnoDockerfile}, agnoBuildFiles...)
			err := extract(bytes.NewReader(makeArchive(t, entries)), t.TempDir(), prefix, names)
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatal(fmt.Sprint(err))
			}
		})
	}
}

func TestAgnoReadinessRequiresReleaseProtocol(t *testing.T) {
	for _, protocol := range []int{2, 3} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			f, _, _ := newAgnoFixture(t)
			dir, err := os.MkdirTemp("/tmp", "agno-probe-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			listener, err := net.Listen("unix", filepath.Join(dir, "agno.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			f.container["Mounts"] = []any{map[string]any{"Type": "bind", "Source": dir, "Destination": "/run/agno"}}
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				var hello map[string]any
				if err := json.NewDecoder(conn).Decode(&hello); err != nil {
					done <- err
					return
				}
				if hello["operation"] != "health" {
					done <- fmt.Errorf("unexpected model request")
					return
				}
				done <- json.NewEncoder(conn).Encode(map[string]any{"type": "health", "protocol": protocol, "ready": true, "capacity": 4, "active_runs": 0, "agno_version": "3.1.2", "openai_version": "3.29.0", "mcp_version": "2.1.0"})
			}()
			c, _, err := inspectService(context.Background(), f.run, f.endpoint, "test")
			if err != nil {
				t.Fatal(err)
			}
			err = probeAgno(context.Background(), c)
			if (err == nil) != (protocol == 3) {
				t.Fatal("wrong release readiness", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlWithoutDockerSkipsOnlyUndeployedAgno(t *testing.T) {
	for _, mode := range []string{"absent", "empty-directory", "custom-socket", "stopped", "socket", "docker-socket", "dangling-lock", "unreadable-path", "relative-socket", "permission-denied", "daemon-unavailable", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			_, db, stage := newAgnoFixture(t)
			dockerSocket := filepath.Join(t.TempDir(), "docker.sock")
			t.Setenv("DOCKER_HOST", "unix://"+dockerSocket)
			t.Setenv("PATH", t.TempDir())
			socket := filepath.Join(db.Directory, "agno", "agno.sock")
			if mode != "absent" {
				if mode == "custom-socket" {
					socket = filepath.Join(t.TempDir(), "agno.sock")
				}
				if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("ALPHA_AGNO_SOCKET", socket)
			}
			switch mode {
			case "stopped":
				writeFile(t, socket+".lock", nil)
			case "socket":
				writeFile(t, socket, nil)
			case "docker-socket":
				writeFile(t, dockerSocket, nil)
			case "dangling-lock":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), socket+".lock"); err != nil {
					t.Fatal(err)
				}
			case "unreadable-path":
				if err := os.Chmod(filepath.Dir(socket), 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(filepath.Dir(socket), 0700) })
				if os.Geteuid() == 0 {
					t.Skip("root bypasses filesystem permission checks")
				}
			case "relative-socket":
				t.Setenv("ALPHA_AGNO_SOCKET", "relative/agno.sock")
			}
			run := dockerCommand(runDocker)
			failure := errors.New(mode)
			if mode == "permission-denied" || mode == "daemon-unavailable" {
				run = func(context.Context, string, []string, []byte) ([]byte, error) { return nil, failure }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			p, err := prepareContainers(ctx, db, stage, run, io.Discard)
			if mode == "absent" || mode == "empty-directory" || mode == "custom-socket" {
				if err != nil || p == nil || len(p.Updates) != 0 {
					t.Fatal("undeployed Agno blocked control update", p, err)
				}
				if err := p.verify(ctx, db, run); err != nil {
					t.Fatal("empty plan verification requires Docker", err)
				}
			} else if err == nil {
				t.Fatal("Docker failure was hidden")
			} else if mode == "permission-denied" || mode == "daemon-unavailable" {
				if !errors.Is(err, failure) {
					t.Fatal("lost Docker failure", err)
				}
			} else if !errors.Is(err, exec.ErrNotFound) {
				t.Fatal("lost missing CLI error", err)
			}
		})
	}
}

func TestControlUpdateWithoutDocker(t *testing.T) {
	// Exercise the target updater's real _migrate child, not a Docker mock.
	target := buildTarget(t)
	for _, sameVersion := range []bool{false, true} {
		t.Run(fmt.Sprint("same-version=", sameVersion), func(t *testing.T) {
			db := oldDatabase(t, "control")
			bin := t.TempDir()
			stage, err := os.MkdirTemp(bin, ".alpha-stage-")
			if err != nil {
				t.Fatal(err)
			}
			const tag = "v0.6.0"
			oldTag := "v0.5.4"
			if sameVersion {
				oldTag = tag
			}
			writeFile(t, filepath.Join(bin, "project-alpha"), script("project-alpha", oldTag))
			writeFile(t, filepath.Join(bin, "alpha-updater"), script("alpha-updater", oldTag))
			prefix := "project-alpha_" + tag + "_linux_" + runtime.GOARCH
			archive := makeArchive(t, agnoArchiveEntries(t, prefix, map[string][]byte{
				prefix + "/bin/project-alpha": script("project-alpha", tag),
				prefix + "/bin/alpha-updater": target,
			}))
			var names []string
			for _, file := range releaseFiles("control") {
				names = append(names, file.source)
			}
			if err := extract(bytes.NewReader(archive), stage, prefix, names); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(stage, "release.tar.gz"), archive)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "docker.sock"))
			t.Setenv("ALPHA_AGNO_SOCKET", filepath.Join(db.Directory, "agno", "agno.sock"))
			if err := os.MkdirAll(filepath.Join(db.Directory, "agno"), 0700); err != nil {
				t.Fatal(err)
			}
			var prepared *PreparedUpdate
			o := options{role: "control", directory: db.Directory, binDir: bin, tag: tag, downloaded: stage,
				prepare: func(p *PreparedUpdate) error { prepared = p; return nil }}
			if err := run(context.Background(), o, github{}, io.Discard); err != nil {
				t.Fatal("prepare", err)
			}
			assertVersion(t, db, 41)
			if sameVersion {
				if prepared != nil {
					t.Fatal("undeployed same-version update scheduled downtime")
				}
			} else {
				if prepared == nil {
					t.Fatal("missing prepared update")
				}
				o.prepare, o.prepared = nil, prepared
				if err := runConfigured(context.Background(), o, io.Discard); err != nil {
					t.Fatal("install/migrate without Docker", err)
				}
				assertVersion(t, db, platform.DatabaseVersion)
			}
			if attempted, err := ServicesAttempted(db.Directory, tag); err != nil || !attempted {
				t.Fatal("missing successful service sync receipt", err)
			}
		})
	}
}

func TestWorkerWithoutDockerStillFails(t *testing.T) {
	_, db, stage := newDockerFixture(t)
	t.Setenv("PATH", t.TempDir())
	_, err := prepareContainers(context.Background(), db, stage, runDocker, io.Discard)
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatal("worker Docker dependency was bypassed", err)
	}
}
