package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
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

type cleanupTarget struct {
	Path, Resolved, Relative string
	Root                     *os.File
}

func closeCleanupTargets(targets []cleanupTarget) {
	for _, target := range targets {
		if target.Root != nil {
			target.Root.Close()
		}
	}
}

func cleanupOverlayForPath(path string, layers []string, mounts []cleanupMount) (*cleanupMount, error) {
	layer := ""
	for _, root := range layers {
		if validateIncrementalPath(root) != nil || root == "/" {
			return nil, fmt.Errorf("容器可写层路径无效")
		}
		if within(root, path) {
			return nil, fmt.Errorf("不能删除容器可写层根及其祖先：%s", path)
		}
		if within(path, root) && len(root) > len(layer) {
			layer = root
		}
	}
	var found *cleanupMount
	for i := range mounts {
		m := &mounts[i]
		if m.FS != "overlay" {
			continue
		}
		// Raw work/lower trees must never be modified by cleanup, including
		// when they are exposed through another explicitly scanned root.
		for _, root := range append(append([]string{}, m.Lower...), m.Work) {
			if root != "" && (within(path, root) || within(root, path)) {
				return nil, fmt.Errorf("不能直接删除 OverlayFS 镜像层或工作目录：%s", path)
			}
		}
		if m.Upper != "" && within(path, m.Upper) && layer != m.Upper {
			return nil, fmt.Errorf("路径属于未记录的容器可写层，请重新扫描：%s", path)
		}
		if layer == "" || m.Upper != layer || m.Root != "/" {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("容器可写层对应多个合并挂载，无法确定删除范围：%s", layer)
		}
		found = m
	}
	if layer != "" && found == nil {
		return nil, fmt.Errorf("容器可写层没有可用的合并挂载，请启动对应容器后重试；不会直接删除 diff 层：%s", layer)
	}
	if found != nil && found.ReadOnly {
		return nil, fmt.Errorf("容器合并挂载只读：%s", found.Path)
	}
	return found, nil
}

func prepareCleanupTargets(request cleanupHelperRequest, mounts []cleanupMount) (targets []cleanupTarget, err error) {
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
		mount, err := cleanupOverlayForPath(path, request.WritableLayers, mounts)
		if err != nil {
			return targets, err
		}
		target := cleanupTarget{Path: path, Resolved: path}
		if mount != nil {
			target.Relative, err = filepath.Rel(mount.Upper, path)
			if err != nil || !filepath.IsLocal(target.Relative) || target.Relative == "." {
				return targets, fmt.Errorf("容器内删除路径无效：%s", path)
			}
			target.Resolved = filepath.Join(mount.Path, target.Relative)
			if err := validateCleanupLocation(request.ProtectedTrees, request.ProtectedRoots, locations, target.Resolved); err != nil {
				return targets, err
			}
		}
		for _, other := range targets {
			if within(path, other.Path) || within(other.Path, path) || within(target.Resolved, other.Resolved) || within(other.Resolved, target.Resolved) {
				return targets, fmt.Errorf("所选删除范围重复或包含父子目录：%s", path)
			}
		}
		if mount != nil {
			fd, err := openCleanupDir(unix.AT_FDCWD, mount.Path, false)
			if err != nil {
				return targets, &os.PathError{Op: "打开容器合并挂载", Path: mount.Path, Err: err}
			}
			target.Root = os.NewFile(uintptr(fd), mount.Path)
			targets = append(targets, target)
			// Pin the actual overlay mount. A stale mountinfo path must not
			// turn into a recursive delete on an ordinary host directory.
			var st unix.Statx_t
			var fs unix.Statfs_t
			if unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &st) != nil || st.Mask&unix.STATX_MNT_ID == 0 || st.Mnt_id != mount.ID || unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.OVERLAYFS_SUPER_MAGIC {
				return targets, fmt.Errorf("容器合并挂载已变化或无法核对，请重新扫描：%s", mount.Path)
			}
		} else {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func (target cleanupTarget) remove(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Root == nil {
		return removeCleanupPath(ctx, target.Path)
	}
	parent, err := unix.Openat2(int(target.Root.Fd()), filepath.Dir(target.Relative), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if os.IsNotExist(err) {
		return nil // The container path is already absent; preserve its whiteout.
	}
	if err != nil {
		return &os.PathError{Op: "打开容器内父目录", Path: filepath.Dir(target.Path), Err: err}
	}
	defer unix.Close(parent)
	return removeCleanupEntry(ctx, parent, filepath.Base(target.Relative), target.Path, true)
}
