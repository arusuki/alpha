package updater

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"project-alpha/internal/platform"
)

const serviceManifest = "node-services.json"

type serviceImage struct {
	Name, Image, Binary, Dockerfile string
	Entrypoint                      []string
}

// Resolved Compose documents contain environment values: keep them private and
// durable. Compose records their paths on replacement containers for later updates.
type containerUpdate struct {
	Name, Service, Project, ID, Image, NextImage string
	Before, After                                json.RawMessage
	Inspection                                   json.RawMessage
	Running                                      bool
}
type containerPlan struct {
	Endpoint string
	Updates  []containerUpdate
}

type dockerCommand func(context.Context, string, []string, []byte) ([]byte, error)

func runDocker(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", endpoint}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	// Explicit endpoint and private resolved Compose files are authoritative.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(key, "COMPOSE_") && !slices.Contains([]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}, key) {
			cmd.Env = append(cmd.Env, env)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		// Compose diagnostics can contain interpolated credentials; do not copy
		// the full resolved document or command output into web-visible errors.
		return nil, fmt.Errorf("docker %s failed: %w; check Docker access and Compose configuration as the worker account (sudo is not invoked)", args[0], err)
	}
	return raw, nil
}

type serviceInspection struct {
	ID, Image, Name string
	Config          struct {
		Labels               map[string]string
		Env, Cmd, Entrypoint []string
		User                 string
	}
	HostConfig struct {
		AutoRemove    bool
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
	}
	State struct {
		Running, Paused, Dead, Restarting bool
		Status                            string
	}
}

func composeStrings(values []string) []string {
	if values == nil {
		return nil
	}
	escaped := make([]string, len(values))
	for i, value := range values {
		escaped[i] = strings.ReplaceAll(value, "$", "$$")
	}
	return escaped
}

func inspectService(ctx context.Context, run dockerCommand, endpoint, id string) (serviceInspection, json.RawMessage, error) {
	var c serviceInspection
	raw, err := run(ctx, endpoint, []string{"container", "inspect", id}, nil)
	if err != nil {
		return c, nil, err
	}
	var all []json.RawMessage
	if err = json.Unmarshal(raw, &all); err != nil || len(all) != 1 {
		return c, nil, errors.New("invalid service container inspection")
	}
	if err = json.Unmarshal(all[0], &c); err != nil {
		return c, nil, err
	}
	// Ignore timestamps, PID and restart counters; changes to configuration,
	// image or desired running state still invalidate an online preparation.
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(all[0], &doc); err != nil {
		return c, nil, err
	}
	snapshot, err := json.Marshal(map[string]any{"Id": c.ID, "Image": c.Image, "Name": c.Name, "Config": doc["Config"], "HostConfig": doc["HostConfig"], "Mounts": doc["Mounts"], "Running": c.State.Running || c.State.Restarting})
	return c, snapshot, err
}

func containerEndpoint(db *platform.Database) (string, error) {
	var raw string
	err := db.SQL.QueryRow("SELECT value FROM container_settings WHERE id=1").Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("read worker Docker configuration: %w", err)
	}
	var c struct{ Endpoint string }
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return "", err
	}
	path := strings.TrimPrefix(c.Endpoint, "unix://")
	if !strings.HasPrefix(c.Endpoint, "unix://") || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\n\r") {
		return "", errors.New("container update requires a local unix:// Docker endpoint")
	}
	return c.Endpoint, nil
}

func prepareContainers(ctx context.Context, db *platform.Database, stage string, run dockerCommand, out io.Writer) (*containerPlan, error) {
	endpoint, err := containerEndpoint(db)
	if err != nil {
		return nil, err
	}
	p := &containerPlan{Endpoint: endpoint, Updates: []containerUpdate{}}
	raw, err := os.ReadFile(filepath.Join(stage, serviceManifest))
	if err != nil {
		return nil, err
	}
	var manifest struct{ Services []serviceImage }
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, image := range manifest.Services {
		if !slices.Contains([]string{"rootless-docker", "dram-bw", "tetragon"}, image.Name) || seen[image.Name] {
			return nil, errors.New("invalid or duplicate service image")
		}
		seen[image.Name] = true
		if image.Image == "" && (!slices.Contains([]string{"rootless-docker", "dram-bwd"}, image.Binary) || image.Dockerfile == "") {
			return nil, errors.New("invalid service build context")
		}
	}
	if len(seen) != 3 {
		return nil, errors.New("release service manifest must describe all three node services")
	}
	for _, image := range manifest.Services {
		raw, err = run(ctx, endpoint, []string{"container", "ls", "--all", "--no-trunc", "--filter", "label=project-alpha.service=" + image.Name, "--format", "{{.ID}}"}, nil)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(raw))
		if len(ids) == 0 {
			fmt.Fprintf(out, "节点服务 %s 未部署，跳过。\n", image.Name)
			continue
		}
		if len(ids) != 1 {
			return nil, fmt.Errorf("%s service container is not unique", image.Name)
		}
		c, snapshot, err := inspectService(ctx, run, endpoint, ids[0])
		if err != nil {
			return nil, err
		}
		if c.ID != ids[0] || c.Config.Labels["project-alpha.service"] != image.Name || c.State.Paused || c.State.Dead || c.State.Status == "removing" || c.HostConfig.AutoRemove {
			return nil, fmt.Errorf("%s container identity/state does not permit updating", image.Name)
		}
		labels := c.Config.Labels
		project, service := labels["com.docker.compose.project"], labels["com.docker.compose.service"]
		if project == "" || service == "" || labels["com.docker.compose.project.config_files"] == "" {
			return nil, fmt.Errorf("%s is not a Compose-managed service; configure its deployment before updating", image.Name)
		}
		args := []string{"compose", "--project-name", project}
		if dir := labels["com.docker.compose.project.working_dir"]; dir != "" {
			args = append(args, "--project-directory", dir)
		}
		for _, env := range strings.Split(labels["com.docker.compose.project.environment_file"], ",") {
			if env != "" {
				args = append(args, "--env-file", env)
			}
		}
		for _, file := range strings.Split(labels["com.docker.compose.project.config_files"], ",") {
			if !filepath.IsAbs(file) {
				return nil, errors.New("Compose config path must be absolute")
			}
			args = append(args, "-f", file)
		}
		raw, err = run(ctx, endpoint, append(args, "config", "--format", "json"), nil)
		if err != nil {
			return nil, err
		}
		var config map[string]any
		if err = json.Unmarshal(raw, &config); err != nil {
			return nil, err
		}
		services, ok := config["services"].(map[string]any)
		if !ok {
			return nil, errors.New("Compose services missing")
		}
		spec, ok := services[service].(map[string]any)
		if !ok {
			return nil, errors.New("Compose service missing")
		}
		// Node services are standalone. Avoid persisting stale image settings
		// for unrelated services into the new deployment's Compose document.
		if deps, ok := spec["depends_on"].(map[string]any); ok && len(deps) > 0 {
			return nil, fmt.Errorf("%s has Compose dependencies; node services must be independently managed", image.Name)
		}
		config["services"] = map[string]any{service: spec}
		// Keep the resolved deployment (mounts, namespaces, logging, GIDs) and
		// reflect runtime settings saved through the node management UI.
		delete(spec, "build")
		delete(spec, "pull_policy")
		delete(spec, "profiles")
		spec["image"] = c.Image
		spec["container_name"] = strings.TrimPrefix(c.Name, "/")
		spec["entrypoint"], spec["command"], spec["user"] = composeStrings(c.Config.Entrypoint), composeStrings(c.Config.Cmd), c.Config.User
		env := map[string]string{}
		for _, item := range c.Config.Env {
			k, v, _ := strings.Cut(item, "=")
			env[k] = strings.ReplaceAll(v, "$", "$$")
		}
		spec["environment"] = env
		restart := c.HostConfig.RestartPolicy.Name
		if restart == "" {
			restart = "no"
		}
		if restart == "on-failure" && c.HostConfig.RestartPolicy.MaximumRetryCount > 0 {
			restart += ":" + strconv.Itoa(c.HostConfig.RestartPolicy.MaximumRetryCount)
		}
		spec["restart"] = restart
		before, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		ref := image.Image
		if ref == "" {
			hash, err := fileHash(filepath.Join(stage, image.Binary))
			if err != nil {
				return nil, err
			}
			ref = "project-alpha-" + image.Name + ":update-" + hash[:16]
			var input bytes.Buffer
			tw := tar.NewWriter(&input)
			binary, err := os.ReadFile(filepath.Join(stage, image.Binary))
			if err != nil {
				return nil, err
			}
			for _, entry := range []struct {
				name string
				data []byte
			}{{"Dockerfile", []byte(image.Dockerfile)}, {image.Binary, binary}} {
				if err = tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0755, Size: int64(len(entry.data))}); err != nil {
					return nil, err
				}
				if _, err = tw.Write(entry.data); err != nil {
					return nil, err
				}
			}
			if err = tw.Close(); err != nil {
				return nil, err
			}
			_, err = run(ctx, endpoint, []string{"build", "--network=none", "--pull=false", "--tag", ref, "-"}, input.Bytes())
		} else {
			_, err = run(ctx, endpoint, []string{"pull", ref}, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("prepare %s image: %w", image.Name, err)
		}
		raw, err = run(ctx, endpoint, []string{"image", "inspect", "--format", "{{.Id}}", ref}, nil)
		if err != nil {
			return nil, err
		}
		next := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(next, "sha256:") || len(next) != 71 {
			return nil, errors.New("invalid prepared image ID")
		}
		spec["image"] = next
		if len(image.Entrypoint) > 0 {
			spec["entrypoint"] = image.Entrypoint
		}
		if image.Name == "dram-bw" {
			labels, _ := spec["labels"].(map[string]any)
			if labels == nil {
				labels = map[string]any{}
			}
			labels["project-alpha.dram-settings"] = "1"
			spec["labels"] = labels
		}
		after, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		p.Updates = append(p.Updates, containerUpdate{Name: image.Name, Service: service, Project: project, ID: c.ID, Image: c.Image, NextImage: next, Before: before, After: after, Inspection: snapshot, Running: c.State.Running || c.State.Restarting})
		fmt.Fprintf(out, "已准备节点服务镜像：%s\n", image.Name)
	}
	return p, nil
}

func (p *containerPlan) verify(ctx context.Context, db *platform.Database, run dockerCommand) error {
	endpoint, err := containerEndpoint(db)
	if err != nil {
		return err
	}
	if endpoint != p.Endpoint {
		return errors.New("Docker endpoint changed after preparation")
	}
	for _, u := range p.Updates {
		raw, err := run(ctx, p.Endpoint, []string{"container", "ls", "--all", "--no-trunc", "--filter", "label=project-alpha.service=" + u.Name, "--format", "{{.ID}}"}, nil)
		if err != nil {
			return err
		}
		if ids := strings.Fields(string(raw)); len(ids) != 1 || ids[0] != u.ID {
			return fmt.Errorf("%s service identity changed after preparation", u.Service)
		}
		c, snapshot, err := inspectService(ctx, run, p.Endpoint, u.ID)
		if err != nil {
			return err
		}
		if c.State.Paused || c.State.Dead || c.State.Status == "removing" || !bytes.Equal(snapshot, u.Inspection) {
			return fmt.Errorf("%s service changed after preparation; prepare again", u.Service)
		}
		for _, id := range []string{u.Image, u.NextImage} {
			if _, err := run(ctx, p.Endpoint, []string{"image", "inspect", id}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// Install uses only prepared image IDs, never a registry or a build. A failed
// replacement restores every attempted service, including the failing one.
func (p *containerPlan) apply(ctx context.Context, directory string, run dockerCommand, out io.Writer) (func() error, error) {
	if len(p.Updates) == 0 {
		return func() error { return nil }, nil
	}
	deployment, err := os.MkdirTemp(directory, ".alpha-service-deploy-")
	if err != nil {
		return nil, err
	}
	for i, u := range p.Updates {
		for _, item := range []struct {
			suffix string
			data   json.RawMessage
		}{{"before", u.Before}, {"after", u.After}} {
			var doc any
			if err = json.Unmarshal(item.data, &doc); err != nil {
				return nil, err
			}
			if err = WriteJSON(filepath.Join(deployment, fmt.Sprintf("%d-%s.json", i, item.suffix)), doc); err != nil {
				return nil, err
			}
		}
	}
	if err = WriteJSON(filepath.Join(deployment, "plan.json"), p); err != nil {
		return nil, err
	}
	if err = syncDirectory(directory); err != nil {
		return nil, err
	}
	// This path is also recoverable if the process dies during Compose up.
	f, err := os.OpenFile(filepath.Join(directory, ".alpha-update-pending"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := fmt.Fprintf(f, "container_plan=%s\n", filepath.Join(deployment, "plan.json"))
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	last := -1
	rollback := func() error {
		recovery, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var errs []error
		for i := last; i >= 0; i-- {
			if err := p.replace(recovery, run, p.Updates[i], filepath.Join(deployment, fmt.Sprintf("%d-before.json", i)), p.Updates[i].Image, false); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) != 0 {
			return fmt.Errorf("%w: container rollback failed: %v; inspect %s", errContainerUncertain, errors.Join(errs...), deployment)
		}
		return nil
	}
	for i, u := range p.Updates {
		last = i
		fmt.Fprintf(out, "更新节点服务：%s（保留原启停状态）\n", u.Service)
		if err = p.replace(ctx, run, u, filepath.Join(deployment, fmt.Sprintf("%d-after.json", i)), u.NextImage, true); err != nil {
			return nil, errors.Join(err, rollback())
		}
	}
	return rollback, nil
}

var errContainerUncertain = errors.New("container update outcome requires inspection")

func (p *containerPlan) replace(ctx context.Context, run dockerCommand, u containerUpdate, file, image string, updated bool) error {
	args := []string{"compose", "--project-name", u.Project, "-f", file}
	if _, err := run(ctx, p.Endpoint, append(slices.Clone(args), "up", "--no-build", "--pull", "never", "--no-deps", "--force-recreate", "--no-start", u.Service), nil); err != nil {
		return err
	}
	if u.Running {
		if _, err := run(ctx, p.Endpoint, append(slices.Clone(args), "start", u.Service), nil); err != nil {
			return err
		}
	}
	raw, err := run(ctx, p.Endpoint, append(slices.Clone(args), "ps", "--all", "--quiet", u.Service), nil)
	if err != nil {
		return err
	}
	ids := strings.Fields(string(raw))
	if len(ids) != 1 {
		return fmt.Errorf("%s replacement container is not unique", u.Service)
	}
	deadline := time.Now().Add(45 * time.Second)
	stable := time.Now()
	for {
		c, _, err := inspectService(ctx, run, p.Endpoint, ids[0])
		if err != nil {
			return err
		}
		if c.Image != image || c.Config.Labels["project-alpha.service"] != u.Name || c.Config.Labels["com.docker.compose.project"] != u.Project || c.Config.Labels["com.docker.compose.service"] != u.Service {
			return errors.New("replacement service identity mismatch")
		}
		if !u.Running {
			if c.State.Running || c.State.Restarting {
				return errors.New("stopped service was unexpectedly started")
			}
			return nil
		}
		if c.State.Running && !c.State.Restarting && !c.State.Dead && !c.State.Paused {
			if time.Since(stable) >= time.Second {
				if u.Name != "rootless-docker" {
					return nil
				}
				// Verify the new manager's actual control protocol, not just a
				// running container. Old-image rollback only requires status.
				probe, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err = run(probe, p.Endpoint, []string{"exec", c.ID, "/rootless-docker", "status"}, nil)
				cancel()
				if err == nil {
					if !updated {
						return nil
					}
					probe, cancel = context.WithTimeout(ctx, 5*time.Second)
					_, err = run(probe, p.Endpoint, []string{"exec", c.ID, "/rootless-docker", "bindings"}, nil)
					cancel()
					if err == nil {
						return nil
					}
				}
			}
		} else {
			stable = time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not remain running after replacement", u.Service)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
