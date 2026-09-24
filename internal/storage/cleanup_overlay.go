package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

type cleanupMount struct {
	ID             uint64
	Root, Path, FS string
	Upper, Work    string
	Lower          []string
	ReadOnly       bool
}

// Read the helper's current namespace, never a merged path supplied by a
// client or inferred by replacing the string "diff" with "merged".
func readCleanupMounts(r io.Reader) ([]cleanupMount, error) {
	var mounts []cleanupMount
	unescape := func(s string) string {
		return mountEscape.ReplaceAllStringFunc(s, func(s string) string {
			v, _ := strconv.ParseUint(s[1:], 8, 8)
			return string(byte(v))
		})
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			left, right, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " - ")
			a, b := strings.Fields(left), strings.Fields(right)
			if !ok || len(a) < 6 || len(b) < 3 {
				return nil, fmt.Errorf("挂载信息格式无效")
			}
			id, parseErr := strconv.ParseUint(a[0], 10, 64)
			if parseErr != nil || id == 0 {
				return nil, fmt.Errorf("挂载标识无效")
			}
			m := cleanupMount{ID: id, Root: unescape(a[3]), Path: unescape(a[4]), FS: b[0]}
			for _, option := range strings.Split(a[5]+","+b[2], ",") {
				if option == "ro" {
					m.ReadOnly = true
				}
				if m.FS != "overlay" {
					continue
				}
				key, value, _ := strings.Cut(option, "=")
				switch key {
				case "upperdir":
					m.Upper = unescape(value)
				case "workdir":
					m.Work = unescape(value)
				case "lowerdir":
					for _, path := range strings.Split(value, ":") {
						if path != "" {
							m.Lower = append(m.Lower, unescape(path))
						}
					}
				}
			}
			mounts = append(mounts, m)
		}
		if err == io.EOF {
			return mounts, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

type cleanupContainer struct {
	ID    string `json:"id"`
	Upper string `json:"upper"`
}

type cleanupTarget struct {
	Path, Resolved string
	Container      *cleanupContainer
	Session        *cleanupDockerSession
	Index          int
}

func closeCleanupTargets(targets []cleanupTarget) {
	seen := map[*cleanupDockerSession]bool{}
	for _, target := range targets {
		if target.Session != nil && !seen[target.Session] {
			seen[target.Session] = true
			target.Session.close()
		}
	}
}

// Only recorded container upper paths may be mapped into docker exec. Raw
// layers remain forbidden even when Docker or the container is unavailable.
func cleanupContainerForPath(path string, containers []cleanupContainer, dockerRoot string, mounts []cleanupMount) (*cleanupContainer, error) {
	if dockerRoot == "" && len(containers) > 0 {
		return nil, fmt.Errorf("缺少 Docker 数据根，请重新扫描")
	}
	if dockerRoot != "" && (validateIncrementalPath(dockerRoot) != nil || dockerRoot == "/") {
		return nil, fmt.Errorf("Docker 数据根无效")
	}
	var found *cleanupContainer
	for i := range containers {
		c := &containers[i]
		if validateIncrementalPath(c.Upper) != nil || c.Upper == "/" || !cleanupContainerID.MatchString(c.ID) {
			return nil, fmt.Errorf("容器可写层标识无效，请重新扫描")
		}
		if c.Upper == dockerRoot || !within(c.Upper, dockerRoot) {
			return nil, fmt.Errorf("容器可写层不在 Docker 数据根内，请重新扫描")
		}
		if within(c.Upper, path) {
			return nil, fmt.Errorf("不能清理容器可写层根及其祖先：%s", path)
		}
		if within(path, c.Upper) {
			if found != nil {
				return nil, fmt.Errorf("容器可写层映射不唯一：%s", path)
			}
			found = c
		}
	}
	for _, m := range mounts {
		if m.FS != "overlay" {
			continue
		}
		for _, root := range append(append([]string{}, m.Lower...), m.Work) {
			if root != "" && (within(path, root) || within(root, path)) {
				return nil, fmt.Errorf("不能直接清理 OverlayFS 镜像层或工作目录：%s", path)
			}
		}
		if m.Upper != "" && (within(path, m.Upper) || within(m.Upper, path)) && (found == nil || found.Upper != m.Upper) {
			return nil, fmt.Errorf("路径属于未记录的容器可写层，请重新扫描：%s", path)
		}
	}
	if dockerRoot != "" {
		if found == nil && (within(path, dockerRoot) || within(dockerRoot, path)) {
			return nil, fmt.Errorf("不能直接清理 Docker 数据目录：%s", path)
		}
	}
	return found, nil
}

func prepareCleanupTargets(ctx context.Context, request cleanupHelperRequest, mounts []cleanupMount, command cleanupCommand) (targets []cleanupTarget, err error) {
	defer func() {
		if err != nil {
			closeCleanupTargets(targets)
		}
	}()
	locations := make([]MountInfo, 0, len(mounts))
	for _, m := range mounts {
		locations = append(locations, MountInfo{Path: m.Path, FS: m.FS})
	}
	for _, path := range request.Paths {
		if err := validateCleanupLocation(request.ProtectedTrees, request.ProtectedRoots, locations, path); err != nil {
			return targets, err
		}
		container, err := cleanupContainerForPath(path, request.Containers, request.DockerRoot, mounts)
		if err != nil {
			return targets, err
		}
		target := cleanupTarget{Path: path, Resolved: path, Container: container}
		if container != nil {
			relative, err := filepath.Rel(container.Upper, path)
			if err != nil || !filepath.IsLocal(relative) || relative == "." {
				return targets, fmt.Errorf("容器内路径无效：%s", path)
			}
			target.Resolved = "/" + filepath.ToSlash(relative)
		}
		for _, other := range targets {
			if within(path, other.Path) || within(other.Path, path) {
				return targets, fmt.Errorf("所选清理范围重复或包含父子目录：%s", path)
			}
		}
		targets = append(targets, target)
	}
	// Every exec worker validates and pins its targets before the first deletion,
	// including batches that mix host directories and multiple containers.
	for i := range targets {
		target := &targets[i]
		if target.Container == nil || target.Session != nil {
			continue
		}
		var paths, protected []string
		var indices []int
		for j := range targets {
			if targets[j].Container != nil && targets[j].Container.ID == target.Container.ID {
				indices = append(indices, j)
				paths = append(paths, targets[j].Resolved)
			}
		}
		for _, tree := range request.ProtectedTrees {
			if within(tree, target.Container.Upper) {
				rel, _ := filepath.Rel(target.Container.Upper, tree)
				protected = append(protected, filepath.Join("/", rel))
			}
		}
		session, err := startCleanupDocker(ctx, *target.Container, paths, protected, command)
		if err != nil {
			return targets, err
		}
		for index, j := range indices {
			targets[j].Session = session
			targets[j].Index = index
		}
	}
	return targets, nil
}

func (target cleanupTarget) remove(ctx context.Context, stats *cleanupStats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Session == nil {
		if target.Container != nil {
			return fmt.Errorf("容器清理进程未准备就绪；不会直接操作可写层")
		}
		return removeCleanupPath(ctx, target.Path, stats)
	}
	err := target.Session.remove(target.Index, stats)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
