package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/fsutil"
)

type Resource struct {
	Path       string   `json:"path"`
	Kinds      []string `json:"kinds"`
	Containers []string `json:"containers"`
}
type ContainerMount struct {
	Type        string  `json:"type"`
	Source      *string `json:"source"`
	Destination string  `json:"destination"`
	RW          bool    `json:"rw"`
}
type Container struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	State         string           `json:"state"`
	Image         string           `json:"image"`
	Owner         string           `json:"owner"`
	SizeRW        *int64           `json:"size_rw"`
	UpperPath     *string          `json:"upper_path"`
	Mounts        []ContainerMount `json:"mounts"`
	LogPath       *string          `json:"log_path"`
	WritableLayer *WritableLayer   `json:"writable_layer,omitempty"`
}
type dockerCommand func(context.Context, []string, int) (string, error)
type workerProcessGroup struct{}

func runDocker(ctx context.Context, args []string, timeout int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	if len(args) >= 2 && args[0] == "--host" {
		// Once discovery resolves its local endpoint, ignore ambient context
		// changes for every subsequent Docker query in this scan.
		cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
			return strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "DOCKER_CONTEXT=")
		})
	}
	// Workers share their isolated group with Docker so the manager can stop all
	// descendants. Standalone scans own a separate group for each CLI invocation.
	if ctx.Value(workerProcessGroup{}) != true {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
	}
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("docker %s: %w", args[0], ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("docker %s failed: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}
func stringPointer(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func appendUnique(values []string, s string) []string {
	for _, v := range values {
		if v == s {
			return values
		}
	}
	return append(values, s)
}

func dockerEndpointSocket(endpoint string) (string, error) {
	if !strings.HasPrefix(endpoint, "unix://") {
		return "", fmt.Errorf("Docker endpoint 必须是本机 unix:// 套接字")
	}
	path := strings.TrimPrefix(endpoint, "unix://")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00?#%") {
		return "", fmt.Errorf("Docker endpoint 必须包含规范的本机套接字绝对路径")
	}
	return path, nil
}

func discoverWithProgress(ctx context.Context, label string, timeout int, command dockerCommand, progress func(object) error) (object, []Container, []Resource, []Warning, error) {
	containers := []Container{}
	resources := []Resource{}
	warnings := []Warning{}
	fail := func(err error) (object, []Container, []Resource, []Warning, error) { return nil, nil, nil, nil, err }
	var ids, volumes []string
	containersKnown, volumesKnown := false, false
	prepared := 0
	report := func(path string, current ...scanContainer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if progress == nil {
			return nil
		}
		v := object{"phase": "discovering", "path": path, "preparation_done": prepared, "preparation_total": nil, "containers_total": nil, "containers_remaining": nil, "containers_discovered": len(containers), "containers_done": 0, "current_containers": append([]scanContainer{}, current...)}
		if containersKnown {
			v["containers_total"], v["containers_remaining"] = len(ids), len(ids)
		}
		if containersKnown && volumesKnown {
			// Four setup steps, then one item per container and volume. This is
			// measured work completed, not an estimate of time or scanned bytes.
			v["preparation_total"] = 4 + len(ids) + len(volumes)
		}
		return progress(v)
	}
	readJSON := func(args []string, v any) error {
		out, err := command(ctx, args, timeout)
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(out), v); err != nil {
			return fmt.Errorf("docker %s invalid JSON: %w", args[0], err)
		}
		return nil
	}
	if err := report("正在确认本机 Docker 连接"); err != nil {
		return fail(err)
	}
	endpoint := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || endpoint == "" {
		var contexts []struct {
			Endpoints map[string]struct{ Host string }
		}
		if err := readJSON([]string{"context", "inspect"}, &contexts); err != nil {
			return fail(err)
		}
		if len(contexts) == 0 {
			return fail(fmt.Errorf("Docker context not found"))
		}
		endpoint = contexts[0].Endpoints["docker"].Host
	}
	if _, err := dockerEndpointSocket(endpoint); err != nil {
		return fail(fmt.Errorf("run on the Docker host with a local unix:// endpoint; remote Docker paths cannot be scanned locally: %w", err))
	}
	baseCommand := command
	command = func(ctx context.Context, args []string, timeout int) (string, error) {
		return baseCommand(ctx, append([]string{"--host", endpoint}, args...), timeout)
	}
	prepared++
	if err := report("正在读取 Docker 存储配置"); err != nil {
		return fail(err)
	}
	var info struct{ ID, DockerRootDir, Driver, ServerVersion string }
	if err := readJSON([]string{"info", "--format", "{{json .}}"}, &info); err != nil {
		return fail(err)
	}
	if info.ID == "" || !filepath.IsAbs(info.DockerRootDir) || filepath.Clean(info.DockerRootDir) != info.DockerRootDir {
		return fail(fmt.Errorf("Docker 返回无效的 daemon 身份或数据目录"))
	}
	metadata := object{"id": info.ID, "endpoint": endpoint, "root": info.DockerRootDir,
		"root_canonical": fsutil.Canonical(info.DockerRootDir), "driver": nil, "version": nil}
	for key, value := range map[string]string{"driver": info.Driver, "version": info.ServerVersion} {
		if value != "" {
			metadata[key] = value
		}
	}
	index := map[string]int{}
	resource := func(path, kind, cid string) *string {
		if path == "" {
			return nil
		}
		path = fsutil.Canonical(path)
		i, ok := index[path]
		if !ok {
			i = len(resources)
			index[path] = i
			resources = append(resources, Resource{path, []string{}, []string{}})
		}
		resources[i].Kinds = appendUnique(resources[i].Kinds, kind)
		if cid != "" {
			resources[i].Containers = appendUnique(resources[i].Containers, cid)
		}
		return &path
	}
	prepared++
	if err := report("正在获取 Docker 容器列表"); err != nil {
		return fail(err)
	}
	// Get names with the cheap container list, before any slow --size query.
	idsRaw, err := command(ctx, []string{"ps", "-a", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}"}, timeout)
	if err != nil {
		return fail(err)
	}
	containerNames := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(idsRaw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return fail(fmt.Errorf("Docker container list is missing an ID or name"))
		}
		ids = append(ids, fields[0])
		containerNames[fields[0]] = fields[1]
	}
	containersKnown = true
	prepared++
	if err := report("正在获取 Docker 数据卷列表"); err != nil {
		return fail(err)
	}
	volumesRaw, err := command(ctx, []string{"volume", "ls", "-q"}, timeout)
	if err != nil {
		return fail(err)
	}
	volumes, volumesKnown = strings.Fields(volumesRaw), true
	prepared++
	// --size can walk a large writable layer inside the Docker daemon. Inspect
	// one container at a time so each completion is visible before the next wait.
	for offset, id := range ids {
		name := containerNames[id]
		if err := report(fmt.Sprintf("正在读取容器 %s 的配置与可写层大小（%d / %d）", name, offset+1, len(ids)), scanContainer{ID: id, Name: name}); err != nil {
			return fail(err)
		}
		var records []struct {
			ID     string `json:"Id"`
			Name   string
			State  struct{ Status string }
			Config struct {
				Image  string
				Labels map[string]string
			}
			GraphDriver struct{ Data map[string]string }
			SizeRW      *int64 `json:"SizeRw"`
			Mounts      []struct {
				Type, Source, Destination string
				RW                        bool
			}
			LogPath string
		}
		if err := readJSON([]string{"container", "inspect", "--size", id}, &records); err != nil {
			return fail(err)
		}
		if len(records) != 1 || records[0].ID != id {
			return fail(fmt.Errorf("Docker did not return the requested container %s", id))
		}
		for _, c := range records {
			if c.ID == "" {
				return fail(fmt.Errorf("Docker container record has no ID"))
			}
			upper := c.GraphDriver.Data["UpperDir"]
			owner, ok := c.Config.Labels[label]
			if !ok {
				owner = "未标注"
			}
			if c.SizeRW != nil && *c.SizeRW < 0 {
				c.SizeRW = nil
			}
			row := Container{ID: c.ID, Name: strings.TrimLeft(c.Name, "/"), State: c.State.Status, Image: c.Config.Image, Owner: owner, SizeRW: c.SizeRW, UpperPath: resource(upper, "writable", c.ID), Mounts: []ContainerMount{}, LogPath: stringPointer(c.LogPath)}
			if upper == "" {
				warnings = append(warnings, Warning{row.Name, "Storage driver does not expose UpperDir; writable layer has Docker SizeRw only (logical bytes), no physical tree"})
			}
			for _, m := range c.Mounts {
				var source *string
				if m.Type == "bind" || m.Type == "volume" {
					source = resource(m.Source, m.Type, c.ID)
				}
				row.Mounts = append(row.Mounts, ContainerMount{m.Type, source, m.Destination, m.RW})
			}
			if c.LogPath != "" {
				resource(filepath.Dir(c.LogPath), "container-data", c.ID)
			}
			containers = append(containers, row)
		}
		prepared++
		if err := report(fmt.Sprintf("已读取容器 %s 的配置与可写层大小（%d / %d）", containers[len(containers)-1].Name, len(containers), len(ids))); err != nil {
			return fail(err)
		}
	}
	for offset := 0; offset < len(volumes); offset += 50 {
		end := offset + 50
		if end > len(volumes) {
			end = len(volumes)
		}
		if err := report(fmt.Sprintf("正在读取第 %d–%d 个 Docker 数据卷的存储路径", offset+1, end)); err != nil {
			return fail(err)
		}
		var records []struct{ Name, Driver, Mountpoint string }
		if err := readJSON(append([]string{"volume", "inspect"}, volumes[offset:end]...), &records); err != nil {
			return fail(err)
		}
		for _, v := range records {
			if v.Driver == "local" && v.Mountpoint != "" {
				resource(v.Mountpoint, "volume", "")
			} else {
				warnings = append(warnings, Warning{v.Name, "Non-local volume is not scanned"})
			}
		}
		prepared += end - offset
		if err := report(fmt.Sprintf("已处理 %d / %d 个 Docker 数据卷", end, len(volumes))); err != nil {
			return fail(err)
		}
	}
	if err := report("容器与数据卷准备完成，即将开始文件扫描"); err != nil {
		return fail(err)
	}
	return metadata, containers, resources, warnings, nil
}
