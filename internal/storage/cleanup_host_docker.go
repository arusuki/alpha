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
	"time"

	"project-alpha/internal/fsutil"
	"project-alpha/internal/httpapi"
)

const localDockerSocket = "/var/run/docker.sock"

// Check the daemon used by the scan. A Docker-free scan still checks the
// default local socket when present, and may proceed when it is absent.
func validateLiveDockerHostPaths(ctx context.Context, paths []string, endpoint, expectedRoot, expectedID string, command cleanupCommand) ([]string, error) {
	if (expectedRoot == "") != (expectedID == "") {
		return nil, httpapi.NewError(503, "扫描记录中的 Docker 身份不完整，Host 删除已拒绝")
	}
	if endpoint == "" {
		if expectedRoot != "" || expectedID != "" {
			return nil, httpapi.NewError(503, "扫描记录缺少 Docker endpoint，Host 删除已拒绝")
		}
		endpoint = "unix://" + localDockerSocket
	}
	socketPath, err := dockerEndpointSocket(endpoint)
	if err != nil {
		return nil, httpapi.NewError(503, "扫描记录中的 Docker endpoint 无效，Host 删除已拒绝")
	}
	info, err := os.Stat(socketPath)
	if os.IsNotExist(err) {
		if expectedRoot != "" || expectedID != "" {
			return nil, httpapi.NewError(503, "无法核对扫描时使用的本机 Docker，Host 删除已拒绝")
		}
		return nil, nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, httpapi.NewError(503, "无法核对本机 Docker 套接字，Host 删除已拒绝")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	protected, err := checkLiveDockerHostPaths(checkCtx, paths, endpoint, expectedRoot, expectedID, command)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return protected, nil
}

// Kept separate from socket detection so tests can supply a mock Docker CLI.
func checkLiveDockerHostPaths(ctx context.Context, paths []string, endpoint, expectedRoot, expectedID string, command cleanupCommand) ([]string, error) {
	if _, err := dockerEndpointSocket(endpoint); err != nil {
		return nil, httpapi.NewError(503, "Docker endpoint 无效，Host 删除已拒绝")
	}
	if command == nil {
		command = exec.CommandContext
	}
	protected := []string{}
	seenProtected := map[string]bool{}
	run := func(args ...string) ([]byte, error) {
		cmd := command(ctx, "docker", append([]string{"--host", endpoint}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C"}
		return cmd.Output()
	}
	protect := func(raw string) error {
		if raw == "" {
			return nil
		}
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			return httpapi.NewError(503, "Docker 返回无效的宿主机路径，Host 删除已拒绝")
		}
		for _, source := range dockerRootAliases(raw) {
			if !seenProtected[source] {
				protected = append(protected, source)
				seenProtected[source] = true
			}
			for _, target := range paths {
				if within(target, source) || within(source, target) {
					return httpapi.NewError(409, "Host 路径当前由本机 Docker 使用，请重新扫描后生成报告："+target)
				}
			}
		}
		return nil
	}
	const infoFormat = `{"id":{{json .ID}},"root":{{json .DockerRootDir}}}`
	output, err := run("info", "--format", infoFormat)
	if err != nil {
		return nil, httpapi.NewError(503, "无法读取本机 Docker 数据目录，Host 删除已拒绝："+err.Error())
	}
	var daemon struct {
		ID   string `json:"id"`
		Root string `json:"root"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(string(output))), &daemon) != nil || daemon.Root == "" ||
		!filepath.IsAbs(daemon.Root) || filepath.Clean(daemon.Root) != daemon.Root {
		return nil, httpapi.NewError(503, "本机 Docker 数据目录格式无效，Host 删除已拒绝")
	}
	if expectedID != "" && daemon.ID != expectedID {
		return nil, httpapi.NewError(409, "本机 Docker daemon 身份与扫描记录不一致，请重新扫描后生成报告")
	}
	if expectedRoot != "" {
		if !filepath.IsAbs(expectedRoot) || filepath.Clean(expectedRoot) != expectedRoot {
			return nil, httpapi.NewError(503, "扫描记录中的 Docker 数据目录无效，Host 删除已拒绝")
		}
		if expectedRoot != fsutil.Canonical(daemon.Root) {
			return nil, httpapi.NewError(409, "本机 Docker 数据目录与扫描记录不一致，请重新扫描后生成报告")
		}
	}
	if err := protect(daemon.Root); err != nil {
		return nil, err
	}
	listIDs := func() ([]string, error) {
		output, err := run("ps", "--all", "--quiet", "--no-trunc")
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if line == "" {
				continue
			}
			if !cleanupContainerID.MatchString(line) {
				return nil, fmt.Errorf("Docker 返回无效的容器 ID")
			}
			ids = append(ids, line)
		}
		slices.Sort(ids)
		for i := 1; i < len(ids); i++ {
			if ids[i] == ids[i-1] {
				return nil, fmt.Errorf("Docker 返回重复的容器 ID")
			}
		}
		return ids, nil
	}
	ids, err := listIDs()
	if err != nil {
		return nil, httpapi.NewError(503, "无法列出本机 Docker 容器，Host 删除已拒绝："+err.Error())
	}
	const inspectFormat = `{"id":{{json .Id}},"upper":{{json .GraphDriver.Data.UpperDir}},"log_path":{{json .LogPath}},"mounts":{{json .Mounts}}}`
	for start := 0; start < len(ids); start += 50 {
		batch := ids[start:min(start+50, len(ids))]
		args := append([]string{"inspect", "--type", "container", "--format", inspectFormat}, batch...)
		output, err := run(args...)
		if err != nil {
			return nil, httpapi.NewError(503, "无法核对本机 Docker 容器存储来源，Host 删除已拒绝："+err.Error())
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) != len(batch) {
			return nil, httpapi.NewError(503, "本机 Docker 容器详情数量不完整，Host 删除已拒绝")
		}
		for i, line := range lines {
			var current struct {
				ID      string `json:"id"`
				Upper   string `json:"upper"`
				LogPath string `json:"log_path"`
				Mounts  []struct {
					Source string `json:"Source"`
				} `json:"mounts"`
			}
			if json.Unmarshal([]byte(line), &current) != nil || current.ID != batch[i] {
				return nil, httpapi.NewError(503, "本机 Docker 容器详情格式无效，Host 删除已拒绝")
			}
			for _, source := range []string{current.Upper, current.LogPath} {
				if err := protect(source); err != nil {
					return nil, err
				}
			}
			for _, mount := range current.Mounts {
				if filepath.IsAbs(mount.Source) {
					if err := protect(mount.Source); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	currentIDs, err := listIDs()
	if err != nil {
		return nil, httpapi.NewError(503, "无法复核本机 Docker 容器列表，Host 删除已拒绝："+err.Error())
	}
	if !slices.Equal(ids, currentIDs) {
		return nil, httpapi.NewError(409, "本机 Docker 容器列表在核对期间变化，请重新扫描后生成报告")
	}
	return protected, nil
}
