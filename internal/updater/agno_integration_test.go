package updater

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Only the isolated Compose project is visible to the updater's discovery.
// Reuses the prebuilt pinned dependencies; the updater builds and deploys a new
// image from an isolated release context without downloading Python wheels again.
func TestDockerAgnoUpdate(t *testing.T) {
	oldImage := os.Getenv("ALPHA_UPDATER_AGNO_TEST_IMAGE")
	if oldImage == "" {
		t.Skip("set ALPHA_UPDATER_AGNO_TEST_IMAGE to an existing Agno test image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	_, db, stage := newAgnoFixture(t)
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	dir, err := os.MkdirTemp("/tmp", "agno-update-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("ALPHA_AGNO_SOCKET", filepath.Join(dir, "agno.sock"))
	file, err := filepath.Abs("../../deploy/control-services.yaml")
	if err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("alpha-agno-update-%d", time.Now().UnixNano())
	envFile := filepath.Join(stage, "initial.env")
	writeFile(t, envFile, []byte(fmt.Sprintf("AGNO_UID=%d\nAGNO_GID=%d\nAGNO_SOCKET_DIR=%s\nAGNO_IMAGE=%s\n", os.Getuid(), os.Getgid(), dir, oldImage)))
	endpoint := "unix:///var/run/docker.sock"
	run := func(ctx context.Context, ep string, args []string, input []byte) ([]byte, error) {
		if slices.Equal(args[:2], []string{"container", "ls"}) {
			args = append(slices.Clone(args), "--filter", "label=com.docker.compose.project="+project)
		}
		return runDocker(ctx, ep, args, input)
	}
	compose := []string{"compose", "--project-name", project, "--env-file", envFile, "-f", file}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if _, err := runDocker(cleanup, endpoint, append(slices.Clone(compose), "down", "--remove-orphans"), nil); err != nil {
			t.Error("cleanup test project", err)
		}
	}()
	if _, err := run(ctx, endpoint, append(slices.Clone(compose), "up", "-d", "--no-build", "--pull", "never", "agno"), nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(dir, "agno.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial Agno did not create socket")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A changed runtime must force a new image even if CI's seed matches HEAD.
	runtimeFile := filepath.Join(stage, "runtime/agno/runtime.py")
	raw, err := os.ReadFile(runtimeFile)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, runtimeFile, append(raw, []byte("\n# isolated updater integration revision\n")...))
	// CI builds the ordinary release Dockerfile before this test. Using that
	// image as the dependency base isolates deployment from registry latency.
	writeFile(t, filepath.Join(stage, agnoDockerfile), []byte("FROM "+oldImage+"\nCOPY runtime/agno/runtime.py runtime/agno/service.py runtime/agno/mcp_runtime.py /app/\nENTRYPOINT [\"python\",\"-I\",\"-u\",\"/app/service.py\"]\n"))
	t.Log("preparing target Agno image")
	p, err := prepareContainers(ctx, db, stage, run, io.Discard)
	if err != nil || len(p.Updates) != 1 {
		t.Fatal(p, err)
	}
	if err := p.verify(ctx, db, run); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, ".alpha-update-pending"), []byte("test\n"))
	t.Log("replacing Agno container and checking release protocol")
	rollback, err := p.apply(ctx, bin, run, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := prepareContainers(ctx, db, stage, run, io.Discard)
	if err != nil || len(repeated.Updates) != 0 {
		t.Fatal("repeat sync recreated Agno", repeated, err)
	}
	t.Log("restoring previous Agno image")
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	ids, err := run(ctx, endpoint, []string{"container", "ls", "--all", "--filter", "label=project-alpha.service=agno", "--format", "{{.ID}}"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := inspectService(ctx, run, endpoint, strings.TrimSpace(string(ids)))
	if err != nil || c.Image != p.Updates[0].Image || !c.State.Running || c.Config.User != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
		t.Fatal("rollback did not preserve running deployment", c, err)
	}
}
