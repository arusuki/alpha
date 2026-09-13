package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"project-alpha/internal/fsutil"
)

// DirectoryInspection specifies physical paths, retained detail and file analysis.
type DirectoryInspection struct {
	StreamDirectory bool
	Paths           []string
	RetainPaths     []string
	Seed            []InodeRecord
	AnalyzeFiles    bool
}

func InspectDirectories(ctx context.Context, c Config, inspection DirectoryInspection, progress func(object) error) (*physicalScan, error) {
	for _, path := range inspection.Paths {
		checked, err := validateDetailPath(path, c, "")
		if err != nil {
			return nil, err
		}
		if checked != path {
			return nil, fmt.Errorf("目录路径已改变，请重新扫描：%s", path)
		}
	}
	if len(inspection.Paths) == 0 {
		return nil, fmt.Errorf("缺少检查目录")
	}
	result, err := collectRequest(ctx, helperRequest{Version: 1, StreamDirectory: inspection.StreamDirectory, Config: c, Paths: inspection.Paths, RetainPaths: inspection.RetainPaths, Seed: inspection.Seed, StrictPaths: true, Mounts: mountTable(), AnalyzeFiles: inspection.AnalyzeFiles}, progress)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Tree == nil {
		return nil, fmt.Errorf("目录检查未返回结果")
	}
	// Verify the paths actually walked as well as the preflight resolution:
	// Scan canonicalizes roots again, and a symlink can change between calls.
	for _, root := range result.Tree.Children {
		matched := false
		for _, path := range inspection.Paths {
			if root.Path == path {
				matched = true
				break
			}
		}
		if !matched || root.Kind == "symlink" {
			return nil, fmt.Errorf("检查期间目录路径已改变：%s", root.Path)
		}
	}
	for _, path := range inspection.Paths {
		covered := false
		for _, root := range result.Tree.Children {
			if within(path, root.Path) {
				covered = true
				break
			}
		}
		if !covered {
			return nil, fmt.Errorf("目录检查遗漏了请求路径：%s", path)
		}
	}
	return result, nil
}

func validateDetailPath(path string, c Config, dataDir string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("细查路径必须是宿主机绝对路径")
	}
	path = fsutil.Canonical(path)
	for _, p := range append(append([]string{}, c.Exclude...), dataDir, "/proc", "/sys", "/dev", "/run") {
		if p != "" && within(path, fsutil.Canonical(p)) {
			return "", fmt.Errorf("该路径已被排除或属于虚拟文件系统")
		}
	}
	if strings.Contains(path, "/merged/") || strings.HasSuffix(path, "/merged") {
		return "", fmt.Errorf("请扫描物理存储路径；不能细查 Docker merged 视图")
	}
	for _, m := range mountTable() {
		if within(path, m.Path) && strings.Contains(" proc sysfs tmpfs devtmpfs overlay squashfs ", " "+m.FS+" ") {
			return "", fmt.Errorf("该路径属于虚拟或合并挂载")
		}
	}
	return path, nil
}
