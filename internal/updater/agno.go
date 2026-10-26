package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"project-alpha/internal/assistant"
)

const agnoDockerfile = "deploy/agno.Dockerfile"

var agnoBuildFiles = []string{
	"runtime/agno/requirements.txt",
	"runtime/agno/mcp-requirements.txt",
	"runtime/agno/runtime.py",
	"runtime/agno/service.py",
	"runtime/agno/mcp_runtime.py",
}

// Match the ordinary Compose build layout, so unchanged dependency layers are
// reusable. Only release-owned sources enter the context, never staged plans,
// environment files, credentials or the control's database.
func buildAgnoImage(ctx context.Context, endpoint, stage, ref string, run dockerCommand) error {
	dir, err := os.MkdirTemp(stage, ".agno-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, name := range append([]string{agnoDockerfile}, agnoBuildFiles...) {
		raw, err := os.ReadFile(filepath.Join(stage, name))
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			return err
		}
		if err := os.Chmod(path, 0644); err != nil {
			return err
		}
	}
	_, err = run(ctx, endpoint, []string{"build", "--pull=false", "--tag", ref, "--file", filepath.Join(dir, agnoDockerfile), dir}, nil)
	return err
}

func controlDockerEndpoint() (string, error) {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	path := strings.TrimPrefix(endpoint, "unix://")
	if !strings.HasPrefix(endpoint, "unix://") || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("Agno update requires a local unix:// DOCKER_HOST")
	}
	return endpoint, nil
}

// A control without container support must still be updatable. Only treat a
// missing CLI as an undeployed service when neither Docker nor Agno has local
// deployment evidence. Agno's lock survives a clean shutdown; its directory
// alone is not evidence because the control's systemd unit creates it eagerly.
// Permission errors, unavailable daemons and broken CLI executions stay errors.
func agnoUndeployedWithoutDocker(directory, endpoint string, dockerErr error) bool {
	if !errors.Is(dockerErr, exec.ErrNotFound) {
		return false
	}
	socket := os.Getenv("ALPHA_AGNO_SOCKET")
	if socket == "" {
		socket = filepath.Join(directory, "agno", "agno.sock")
	}
	if !filepath.IsAbs(socket) {
		return false
	}
	for _, path := range []string{strings.TrimPrefix(endpoint, "unix://"), socket, socket + ".lock"} {
		// Lstat also treats dangling symlinks as evidence; only definite absence
		// is safe to skip, never an unreadable path or an unexpected file type.
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func agnoSocket(c serviceInspection) (string, error) {
	for _, mount := range c.Mounts {
		if mount.Type == "bind" && mount.Destination == "/run/agno" && filepath.IsAbs(mount.Source) {
			return filepath.Join(mount.Source, "agno.sock"), nil
		}
	}
	return "", fmt.Errorf("Agno requires a bind mount at /run/agno")
}

func validateAgnoMount(directory string, c serviceInspection) error {
	socket, err := agnoSocket(c)
	if err != nil {
		return err
	}
	expected := os.Getenv("ALPHA_AGNO_SOCKET")
	if expected == "" {
		expected = filepath.Join(directory, "agno", "agno.sock")
	}
	if !filepath.IsAbs(expected) || filepath.Clean(expected) != socket {
		return fmt.Errorf("Agno container socket does not match this control's ALPHA_AGNO_SOCKET or data directory; refusing to update another deployment")
	}
	return nil
}

func validateAgnoCompose(spec map[string]any, c serviceInspection) error {
	socket, err := agnoSocket(c)
	if err != nil {
		return err
	}
	volumes, _ := spec["volumes"].([]any)
	for _, raw := range volumes {
		mount, _ := raw.(map[string]any)
		if mount["target"] == "/run/agno" && mount["type"] == "bind" && mount["source"] == filepath.Dir(socket) {
			return nil
		}
	}
	return fmt.Errorf("Agno Compose socket mount differs from its container; restore the deployment configuration before updating")
}

// Initial Compose deployments commonly receive AGNO_* only from an interactive
// shell. Recover these four template values from the existing container, so the
// service updater does not require that shell environment after a restart.
func agnoComposeEnvironment(stage string, c serviceInspection) (string, error) {
	socket, err := agnoSocket(c)
	if err != nil {
		return "", err
	}
	uid, gid, ok := strings.Cut(c.Config.User, ":")
	u, eu := strconv.Atoi(uid)
	g, eg := strconv.Atoi(gid)
	if !ok || eu != nil || eg != nil || u <= 0 || g < 0 {
		return "", fmt.Errorf("Agno container must use its existing non-root numeric UID:GID")
	}
	dir := filepath.Dir(socket)
	if strings.ContainsAny(dir+c.Image, "'\r\n\x00") {
		return "", fmt.Errorf("invalid Agno deployment environment")
	}
	f, err := os.CreateTemp(stage, ".agno-compose-env-")
	if err != nil {
		return "", err
	}
	_, writeErr := fmt.Fprintf(f, "AGNO_UID=%d\nAGNO_GID=%d\nAGNO_SOCKET_DIR='%s'\nAGNO_IMAGE='%s'\n", u, g, dir, c.Image)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write Agno deployment environment: %v %v", writeErr, closeErr)
	}
	return f.Name(), nil
}

func probeAgno(ctx context.Context, c serviceInspection) error {
	socket, err := agnoSocket(c)
	if err != nil {
		return err
	}
	health := assistant.CheckRuntime(ctx, socket)
	if health.Status != "ready" {
		return fmt.Errorf("Agno is not ready for this release: %s", health.Detail)
	}
	return nil
}
