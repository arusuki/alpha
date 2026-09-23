package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"project-alpha/internal/fsutil"
	"project-alpha/internal/httpapi"
)

// Only the report workflow calls this capability, never a model tool. Serialize
// with scans and record deletion so evidence cannot disappear during a batch.
func (s *Service) DeleteReportPaths(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
	defer clear(password)
	s.Manager.mu.Lock()
	defer s.Manager.mu.Unlock()
	if s.Manager.closed {
		return httpapi.NewError(503, "存储服务正在关闭")
	}
	if err := s.Manager.reapLocked(); err != nil {
		return err
	}
	if s.Manager.process != nil {
		return httpapi.NewError(409, "扫描正在执行，请结束后重试")
	}
	snapshot, err := s.ReadSnapshot(id)
	if err != nil {
		return err
	}
	config, err := s.DB.config()
	if err != nil {
		return err
	}
	mountFile, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	mounts, err := parseMountTable(mountFile)
	mountFile.Close()
	if err != nil {
		return err
	}
	// Validate the whole selection before deleting the first entry.
	for _, p := range paths {
		if err := validateCleanupPath(snapshot, s.DB.Directory, config.Value.Exclude, mounts, p); err != nil {
			for _, target := range paths {
				if saveErr := result(target, "failed", err.Error()); saveErr != nil {
					return saveErr
				}
			}
			return nil
		}
	}
	request := cleanupHelperRequest{Paths: paths}
	request.ProtectedTrees, request.ProtectedRoots = cleanupProtectedPaths(snapshot, s.DB.Directory, config.Value.Exclude)
	for _, c := range snapshot.Containers {
		if c.UpperPath != nil && *c.UpperPath != "" {
			request.WritableLayers = appendUnique(request.WritableLayers, *c.UpperPath)
		}
	}
	return runSudoCleanup(ctx, password, request, result, nil)
}

func validateCleanupPath(snapshot *Snapshot, dataDir string, excludes []string, mounts []MountInfo, p string) error {
	if err := validateIncrementalPath(p); err != nil {
		return err
	}
	reject := func() error { return httpapi.NewError(400, "不能删除受保护、未记录或非物理路径："+p) }
	node := findSnapshotNode(snapshot.Tree, p)
	if node == nil || (node.Kind != "directory" && node.Kind != "file") {
		return reject()
	}
	trees, roots := cleanupProtectedPaths(snapshot, dataDir, excludes)
	return validateCleanupLocation(trees, roots, mounts, p)
}

func cleanupProtectedPaths(snapshot *Snapshot, dataDir string, excludes []string) ([]string, []string) {
	trees := append([]string{dataDir}, excludes...)
	switch values := snapshot.Scan["excludes"].(type) {
	case []string:
		trees = append(trees, values...)
	case []any:
		for _, v := range values {
			if p, ok := v.(string); ok {
				trees = append(trees, p)
			}
		}
	}
	roots := []string{}
	if root, ok := snapshot.Docker["root"].(string); ok && root != "" {
		roots = append(roots, root)
	}
	for _, c := range snapshot.Containers {
		if c.UpperPath != nil && *c.UpperPath != "" {
			roots = append(roots, *c.UpperPath)
			// Docker installs these mounts in the container's namespace. They
			// need not appear below the host's merged mount in mountinfo.
			for _, mount := range c.Mounts {
				if filepath.IsAbs(mount.Destination) {
					trees = append(trees, filepath.Join(*c.UpperPath, mount.Destination))
				}
			}
		}
	}
	return trees, roots
}

func validateCleanupLocation(trees, roots []string, mounts []MountInfo, p string) error {
	if err := validateIncrementalPath(p); err != nil {
		return err
	}
	reject := func() error { return httpapi.NewError(400, "不能删除受保护路径："+p) }
	if p == "/" || filepath.Dir(p) == "/" {
		return reject()
	}
	for _, root := range []string{"/proc", "/sys", "/dev", "/boot", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/usr"} {
		if within(p, root) || within(root, p) {
			return reject()
		}
	}
	for _, root := range trees {
		root = fsutil.Canonical(root)
		if within(p, root) || within(root, p) {
			return reject()
		}
	}
	for _, root := range roots {
		if within(fsutil.Canonical(root), p) {
			return reject()
		}
	}
	for _, mount := range mounts {
		if within(mount.Path, p) {
			return reject()
		}
	}
	return nil
}

func openCleanupDir(parent int, name string, noCrossMount bool) (int, error) {
	resolve := uint64(unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS)
	if noCrossMount {
		resolve |= unix.RESOLVE_NO_XDEV
	}
	return unix.Openat2(parent, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolve})
}

func removeCleanupPath(ctx context.Context, p string) error {
	// Anchor every operation to directory descriptors; no shell, symlink traversal
	// or path-based recursive RemoveAll. openat2 also rejects nested bind mounts.
	parent, err := openCleanupDir(unix.AT_FDCWD, filepath.Dir(p), false)
	if err != nil {
		return &os.PathError{Op: "打开父目录（openat2）", Path: filepath.Dir(p), Err: err}
	}
	defer unix.Close(parent)
	return removeCleanupEntry(ctx, parent, filepath.Base(p), p, false)
}

func removeCleanupEntry(ctx context.Context, parent int, name, p string, missingOK bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if missingOK && os.IsNotExist(err) {
			return nil // Already absent from the merged view, e.g. a whiteout.
		}
		return &os.PathError{Op: "检查删除目标", Path: p, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return &os.PathError{Op: "检查删除目标", Path: p, Err: fmt.Errorf("报告路径已变为符号链接")}
	}
	return removeCleanupAt(ctx, parent, name, p)
}

// p is only used in diagnostics. All filesystem operations stay anchored to
// parent/name, including children whose full display path is assembled below.
func removeCleanupAt(ctx context.Context, parent int, name, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var before unix.Stat_t
	if err := unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "检查文件", Path: p, Err: err}
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFREG, unix.S_IFLNK, unix.S_IFSOCK, unix.S_IFIFO:
			// Unlink only the directory entry: never open/connect to IPC files.
		case unix.S_IFCHR:
			return &os.PathError{Op: "检查文件", Path: p, Err: fmt.Errorf("目录包含字符设备，已停止删除")}
		case unix.S_IFBLK:
			return &os.PathError{Op: "检查文件", Path: p, Err: fmt.Errorf("目录包含块设备，已停止删除")}
		default:
			return &os.PathError{Op: "检查文件", Path: p, Err: fmt.Errorf("不支持的文件类型：%#o", before.Mode&unix.S_IFMT)}
		}
		if err := unix.Unlinkat(parent, name, 0); err != nil {
			return &os.PathError{Op: "删除文件", Path: p, Err: err}
		}
		return nil
	}
	fd, err := openCleanupDir(parent, name, true)
	if err != nil {
		return &os.PathError{Op: "打开目录（openat2）", Path: p, Err: err}
	}
	dir := os.NewFile(uintptr(fd), p)
	defer dir.Close()
	var opened unix.Stat_t
	if err = unix.Fstat(fd, &opened); err != nil {
		return &os.PathError{Op: "检查已打开目录", Path: p, Err: err}
	}
	if opened.Dev != before.Dev || opened.Ino != before.Ino {
		return &os.PathError{Op: "检查已打开目录", Path: p, Err: fmt.Errorf("删除期间目录已改变")}
	}
	for {
		names, err := dir.Readdirnames(128)
		for _, child := range names {
			if err := removeCleanupAt(ctx, fd, child, filepath.Join(p, child)); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	var current unix.Stat_t
	if err = unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "复查目录", Path: p, Err: err}
	}
	if current.Dev != opened.Dev || current.Ino != opened.Ino {
		return &os.PathError{Op: "复查目录", Path: p, Err: fmt.Errorf("删除期间目录已替换")}
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil {
		return &os.PathError{Op: "删除目录", Path: p, Err: err}
	}
	return nil
}
