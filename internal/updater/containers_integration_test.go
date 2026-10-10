package updater

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Uses one isolated Compose project with the mock DRAM backend. The label query
// is additionally scoped so this test cannot update the host's actual services.
func TestDockerContainerUpdate(t *testing.T) {
	if os.Getenv("ALPHA_UPDATER_DOCKER_TEST") != "1" {
		t.Skip("set ALPHA_UPDATER_DOCKER_TEST=1 for isolated Docker smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	stage := t.TempDir()
	build := filepath.Join(dir, "build")
	cmd := exec.CommandContext(ctx, "make", "-C", "../../ctools/dram-bw", "BUILD="+build, "LDFLAGS=-static", filepath.Join(build, "dram-bwd"))
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build static daemon: %v: %s", err, raw)
	}
	binary, err := os.ReadFile(filepath.Join(build, "dram-bwd"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stage, "dram-bwd"), binary)
	manifest, err := os.ReadFile("../../deploy/updater-services.json")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stage, serviceManifest), manifest)
	project := fmt.Sprintf("alpha-updater-test-%d", time.Now().UnixNano())
	endpoint := "unix:///var/run/docker.sock"
	run := func(ctx context.Context, ep string, args []string, input []byte) ([]byte, error) {
		if len(args) > 1 && slices.Equal(args[:2], []string{"container", "ls"}) {
			args = append(slices.Clone(args), "--filter", "label=com.docker.compose.project="+project)
		}
		return runDocker(ctx, ep, args, input)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"Dockerfile", []byte("FROM scratch\nCOPY dram-bwd /dram-bwd\nENTRYPOINT [\"/dram-bwd\"]\n")}, {"dram-bwd", binary}} {
		if err = tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0755, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	oldTag := project + ":old"
	if _, err = run(ctx, endpoint, []string{"build", "--network=none", "--tag", oldTag, "-"}, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	oldRaw, err := run(ctx, endpoint, []string{"image", "inspect", "--format", "{{.Id}}", oldTag}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldImage := strings.TrimSpace(string(oldRaw))
	file := filepath.Join(dir, "compose.json")
	data := filepath.Join(dir, "data")
	if err = os.Mkdir(data, 0755); err != nil {
		t.Fatal(err)
	}
	saved := []byte("mock 2000 0\n")
	if err = os.WriteFile(filepath.Join(data, "service-settings"), saved, 0644); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"services": map[string]any{"dram-bw": map[string]any{"image": oldTag, "container_name": project, "network_mode": "none", "restart": "unless-stopped", "user": fmt.Sprintf("0:%d", os.Getgid()), "labels": map[string]string{"project-alpha.service": "dram-bw"}, "environment": map[string]string{"LITERAL": "literal$$secret"}, "volumes": []string{data + ":/run/dram-bw"}, "logging": map[string]any{"driver": "json-file", "options": map[string]string{"max-size": "10m", "max-file": "3"}}, "command": []string{"--backend", "mock", "--interval-us", "1000", "--socket", "/run/dram-bw/control.sock"}}}}
	if err = WriteJSON(file, config); err != nil {
		t.Fatal(err)
	}
	compose := []string{"compose", "--project-name", project, "-f", file}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := run(cleanup, endpoint, []string{"container", "rm", "--force", project}, nil); err != nil {
			t.Error(err)
		}
		_, _ = run(cleanup, endpoint, []string{"image", "rm", oldTag}, nil)
	})
	if _, err = run(ctx, endpoint, append(slices.Clone(compose), "up", "-d", "--no-build", "--pull", "never"), nil); err != nil {
		t.Fatal(err)
	}
	db := oldDatabase(t, "worker")
	if _, err = db.SQL.Exec("UPDATE container_settings SET value=? WHERE id=1", `{"endpoint":"unix:///var/run/docker.sock"}`); err != nil {
		t.Fatal(err)
	}
	for _, running := range []bool{true, false} {
		if !running {
			if _, err = run(ctx, endpoint, []string{"stop", project}, nil); err != nil {
				t.Fatal(err)
			}
		}
		p, err := prepareContainers(ctx, db, stage, run, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Updates) != 1 || p.Updates[0].Running != running {
			t.Fatal(p)
		}
		if err = p.verify(ctx, db, run); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, ".alpha-update-pending"), []byte("integration\n"))
		rollback, err := p.apply(ctx, dir, run, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := inspectService(ctx, run, endpoint, project)
		if err != nil {
			t.Fatal(err)
		}
		if c.Image == oldImage || c.State.Running != running || !slices.Contains(c.Config.Env, "LITERAL=literal$secret") || c.Config.User != fmt.Sprintf("0:%d", os.Getgid()) || c.Config.Labels["project-alpha.dram-settings"] != "1" {
			t.Fatalf("deployment changed: %+v", c)
		}
		// The persistent generated file must be usable by the next updater run.
		if _, err = prepareContainers(ctx, db, stage, run, io.Discard); err != nil {
			t.Fatalf("repeat preparation: %v", err)
		}
		preserved, err := os.ReadFile(filepath.Join(data, "service-settings"))
		if err != nil || !bytes.Equal(saved, preserved) {
			t.Fatal("lost saved parameters", err)
		}
		raw, err := run(ctx, endpoint, []string{"inspect", project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var inspected []struct {
			HostConfig struct {
				LogConfig struct{ Config map[string]string }
			}
		}
		if err = json.Unmarshal(raw, &inspected); err != nil || inspected[0].HostConfig.LogConfig.Config["max-size"] != "10m" {
			t.Fatal("lost log rotation", err)
		}
		if err = rollback(); err != nil {
			t.Fatal(err)
		}
		c, _, err = inspectService(ctx, run, endpoint, project)
		if err != nil || c.Image != oldImage || c.State.Running != running {
			t.Fatal("rollback did not restore deployment", err)
		}
	}
}
