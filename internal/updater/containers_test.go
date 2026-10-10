package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

type dockerFixture struct {
	t                  *testing.T
	container          map[string]any
	compose            map[string]any
	calls              [][]string
	old, next          string
	fail, rollbackFail bool
	duplicate, denied  bool
}

func noDeployedServices(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
	if len(args) > 1 && slices.Equal(args[:2], []string{"container", "ls"}) {
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected Docker operation on a worker without services: %v", args)
}

func newDockerFixture(t *testing.T) (*dockerFixture, *platform.Database, string) {
	t.Helper()
	db := oldDatabase(t, "worker")
	stage := t.TempDir()
	manifest, err := os.ReadFile("../../deploy/updater-services.json")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stage, serviceManifest), manifest)
	writeFile(t, filepath.Join(stage, "dram-bwd"), []byte("dram binary"))
	writeFile(t, filepath.Join(stage, "rootless-docker"), []byte("rootless binary"))
	f := &dockerFixture{t: t, old: "sha256:" + strings.Repeat("a", 64), next: "sha256:" + strings.Repeat("b", 64)}
	f.container = map[string]any{
		"Id": strings.Repeat("c", 64), "Image": f.old, "Name": "/dram-bw",
		"Config":     map[string]any{"Labels": map[string]string{"project-alpha.service": "dram-bw", "com.docker.compose.project": "node-services", "com.docker.compose.service": "dram-bw", "com.docker.compose.project.config_files": "/deploy/services.yaml,/deploy/override.yaml", "com.docker.compose.project.environment_file": "/deploy/.env", "com.docker.compose.project.working_dir": "/deploy"}, "Entrypoint": []string{"/old/dram"}, "Cmd": []string{"--backend", "mock"}, "Env": []string{"TOKEN=literal$secret"}, "User": "0:1005"},
		"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 3}},
		"Mounts":     []any{map[string]any{"Source": "/run/dram-bw", "Destination": "/run/dram-bw"}},
		"State":      map[string]any{"Status": "exited", "Running": false},
	}
	f.compose = map[string]any{"name": "node-services", "services": map[string]any{"dram-bw": map[string]any{"image": "old-tag", "build": map[string]any{"context": "/old/source"}, "privileged": true, "network_mode": "none", "user": "0:0", "restart": "always", "labels": map[string]any{"project-alpha.service": "dram-bw"}, "volumes": []any{map[string]any{"type": "bind", "source": "/run/dram-bw", "target": "/run/dram-bw"}}, "logging": map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"}}}}}
	return f, db, stage
}

func (f *dockerFixture) run(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
	f.t.Helper()
	f.calls = append(f.calls, slices.Clone(args))
	if endpoint != "unix:///test/docker.sock" {
		f.t.Fatal(endpoint)
	}
	if f.denied {
		return nil, errors.New("Docker permission denied")
	}
	marshal := func(value any) ([]byte, error) { return json.Marshal(value) }
	if slices.Equal(args[:2], []string{"container", "ls"}) {
		if !slices.Contains(args, "label=project-alpha.service=dram-bw") {
			return nil, nil
		}
		id := f.container["Id"].(string)
		if f.duplicate {
			id += "\n" + strings.Repeat("d", 64)
		}
		return []byte(id), nil
	}
	if slices.Equal(args[:2], []string{"container", "inspect"}) {
		return marshal([]any{f.container})
	}
	if args[0] == "build" {
		if !bytes.Contains(input, []byte("FROM scratch")) || !bytes.Contains(input, []byte("dram binary")) {
			f.t.Fatal("missing offline build context")
		}
		return nil, nil
	}
	if args[0] == "image" {
		return []byte(f.next + "\n"), nil
	}
	if args[0] == "compose" {
		if slices.Contains(args, "config") {
			if !slices.Contains(args, "/deploy/.env") || !slices.Contains(args, "/deploy/override.yaml") {
				f.t.Fatal("lost deployment inputs", args)
			}
			return marshal(f.compose)
		}
		if slices.Contains(args, "up") {
			for _, required := range []string{"--no-build", "never", "--no-deps", "--no-start"} {
				if !slices.Contains(args, required) {
					f.t.Fatal(args)
				}
			}
			path := args[slices.Index(args, "-f")+1]
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			var config map[string]any
			if err = json.Unmarshal(raw, &config); err != nil {
				return nil, err
			}
			spec := config["services"].(map[string]any)["dram-bw"].(map[string]any)
			image := spec["image"].(string)
			f.container["Image"] = image
			f.container["Id"] = strings.Repeat("e", 64)
			f.container["State"] = map[string]any{"Status": "created", "Running": false}
			if image == f.next && f.fail || image == f.old && f.rollbackFail {
				return nil, errors.New("injected recreate failure")
			}
			return nil, nil
		}
		if slices.Contains(args, "start") {
			f.container["State"] = map[string]any{"Status": "running", "Running": true}
			return nil, nil
		}
		if slices.Contains(args, "ps") {
			return []byte(f.container["Id"].(string)), nil
		}
	}
	return nil, fmt.Errorf("unexpected Docker command %v", args)
}

func TestContainerUpdatePreservesDeployment(t *testing.T) {
	f, db, stage := newDockerFixture(t)
	p, err := prepareContainers(context.Background(), db, stage, f.run, io.Discard)
	if err != nil || len(p.Updates) != 1 {
		t.Fatal(p, err)
	}
	for _, args := range f.calls {
		if slices.Contains(args, "up") || slices.Contains(args, "start") {
			t.Fatal("preparation stopped a service", args)
		}
	}
	var doc map[string]any
	if err = json.Unmarshal(p.Updates[0].After, &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc["services"].(map[string]any)["dram-bw"].(map[string]any)
	if spec["image"] != f.next || spec["user"] != "0:1005" || spec["restart"] != "on-failure:3" || spec["privileged"] != true || spec["network_mode"] != "none" || spec["build"] != nil || spec["logging"] == nil || spec["volumes"] == nil {
		t.Fatal(spec)
	}
	if spec["environment"].(map[string]any)["TOKEN"] != "literal$$secret" {
		t.Fatal("Compose would interpolate a literal dollar", spec)
	}
	if spec["entrypoint"].([]any)[1] != "--settings" || spec["labels"].(map[string]any)["project-alpha.dram-settings"] != "1" {
		t.Fatal("old DRAM entrypoint retained", spec)
	}
	if err = p.verify(context.Background(), db, f.run); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".alpha-update-pending"), []byte("journal\n"))
	rollback, err := p.apply(context.Background(), dir, f.run, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.container["Image"] != f.next {
		t.Fatal("image not updated")
	}
	for _, args := range f.calls {
		if slices.Contains(args, "start") {
			t.Fatal("stopped service started")
		}
	}
	journal, _ := os.ReadFile(filepath.Join(dir, ".alpha-update-pending"))
	if !bytes.Contains(journal, []byte("container_plan=")) {
		t.Fatal("missing recovery plan")
	}
	if err = rollback(); err != nil || f.container["Image"] != f.old {
		t.Fatal("rollback failed", err)
	}
}

func TestContainerFailuresAndDrift(t *testing.T) {
	for _, mode := range []string{"denied", "duplicate", "drift", "recreate", "rollback", "migration", "running"} {
		t.Run(mode, func(t *testing.T) {
			f, db, stage := newDockerFixture(t)
			f.denied = mode == "denied"
			f.duplicate = mode == "duplicate"
			if mode == "running" {
				f.container["State"] = map[string]any{"Status": "running", "Running": true}
			}
			p, err := prepareContainers(context.Background(), db, stage, f.run, io.Discard)
			if f.denied || f.duplicate {
				if err == nil {
					t.Fatal("invalid deployment accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "drift" {
				f.container["Config"].(map[string]any)["User"] = "0:2000"
				if err = p.verify(context.Background(), db, f.run); err == nil {
					t.Fatal("configuration drift ignored")
				}
				return
			}
			f.fail = mode != "running" && mode != "migration"
			f.rollbackFail = mode == "rollback"
			dir, bin := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(dir, "project-alpha"), []byte("old"))
			writeFile(t, filepath.Join(bin, "project-alpha"), []byte("new"))
			err = install(dir, bin, []binary{{"project-alpha", "project-alpha"}}, "backup", func() error {
				rollback, err := p.apply(context.Background(), dir, f.run, io.Discard)
				if err == nil && mode == "migration" {
					return errors.Join(errors.New("migration failed"), rollback())
				}
				return err
			}, io.Discard)
			body, _ := os.ReadFile(filepath.Join(dir, "project-alpha"))
			_, pending := os.Stat(filepath.Join(dir, ".alpha-update-pending"))
			switch mode {
			case "running":
				if err != nil || f.container["State"].(map[string]any)["Running"] != true || string(body) != "new" || !os.IsNotExist(pending) {
					t.Fatal(err, string(body), pending)
				}
			case "recreate", "migration":
				if err == nil || f.container["Image"] != f.old || string(body) != "old" || !os.IsNotExist(pending) {
					t.Fatal(err, string(body), pending)
				}
			case "rollback":
				if !errors.Is(err, errContainerUncertain) || string(body) != "new" || pending != nil {
					t.Fatal("lost uncertain recovery state", err, string(body), pending)
				}
			}
		})
	}
}

func TestServicesOnlyPreservesBinariesAndDatabase(t *testing.T) {
	db := oldDatabase(t, "worker")
	dir := t.TempDir()
	for _, file := range binaries("worker") {
		writeFile(t, filepath.Join(dir, file.destination), script(file.source, "v0.6.0"))
	}
	g := releaseServer(t, script("alpha-updater", "v0.6.0"), false)
	if err := run(context.Background(), options{role: "worker", directory: db.Directory, binDir: dir, servicesOnly: true, docker: noDeployedServices}, g, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, db, 37)
	for _, file := range binaries("worker") {
		raw, err := os.ReadFile(filepath.Join(dir, file.destination))
		if err != nil || !bytes.Equal(raw, script(file.source, "v0.6.0")) {
			t.Fatal("services-only changed executable", file, err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
	if len(backups) != 0 {
		t.Fatal("services-only touched database", backups)
	}
	writeFile(t, filepath.Join(dir, "project-alpha"), script("project-alpha", "v0.5.4"))
	if err := run(context.Background(), options{role: "worker", directory: db.Directory, binDir: dir, servicesOnly: true, docker: noDeployedServices}, g, io.Discard); err == nil || !strings.Contains(err.Error(), "requires the installed version") {
		t.Fatal("services-only accepted a mismatched release", err)
	}
}

func TestContainerPayloadHash(t *testing.T) {
	stage := t.TempDir()
	p := &PreparedUpdate{Stage: stage, Hashes: map[string]string{}}
	for _, file := range releaseFiles("worker") {
		writeFile(t, filepath.Join(stage, file.source), []byte("original"))
		hash, err := fileHash(filepath.Join(stage, file.source))
		if err != nil {
			t.Fatal(err)
		}
		p.Hashes[file.source] = hash
	}
	writeFile(t, filepath.Join(stage, serviceManifest), []byte("changed"))
	if err := p.verify(releaseFiles("worker")); err == nil {
		t.Fatal("changed container payload accepted")
	}
}

func TestRootlessUpdateVerifiesBindings(t *testing.T) {
	for _, updated := range []bool{false, true} {
		t.Run(fmt.Sprint(updated), func(t *testing.T) {
			f, _, _ := newDockerFixture(t)
			// Exercise the status protocol after Compose has started the replacement.
			f.container["State"] = map[string]any{"Running": true, "Status": "running"}
			f.container["Image"] = f.next
			labels := f.container["Config"].(map[string]any)["Labels"].(map[string]string)
			labels["project-alpha.service"], labels["com.docker.compose.service"] = "rootless-docker", "rootless-docker"
			u := containerUpdate{Name: "rootless-docker", Service: "rootless-docker", Project: "node-services", Running: true}
			dir, err := os.MkdirTemp("/tmp", "sync-probe-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			listener, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			probes := make(chan string, 4)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Args []string }
				if r.Method != "POST" || r.URL.Path != "/_rootless/control" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Args) != 1 {
					t.Error("invalid control request")
					w.WriteHeader(400)
					return
				}
				probes <- request.Args[0]
				fmt.Fprint(w, `{"output":"[]"}`)
			})}
			go server.Serve(listener)
			defer server.Close()
			f.container["Mounts"] = []any{map[string]any{"Type": "bind", "Source": dir, "Destination": "/run/rootless-docker"}}
			run := func(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
				if args[0] == "compose" && !slices.Contains(args, "ps") {
					return nil, nil
				}
				return f.run(ctx, endpoint, args, input)
			}
			p := containerPlan{Endpoint: "unix:///test/docker.sock"}
			if err := p.replace(context.Background(), run, u, "unused.json", f.next, updated); err != nil {
				t.Fatal(err)
			}
			want := []string{"status"}
			if updated {
				want = append(want, "bindings")
			}
			var got []string
			for len(probes) > 0 {
				got = append(got, <-probes)
			}
			if !slices.Equal(got, want) {
				t.Fatal(got, want)
			}
		})
	}
}
