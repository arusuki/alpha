package rootless

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func confinedOpen(root int, name string, flags int, mode uint32) (int, error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return -1, errors.New("热挂载目前支持 x86_64/aarch64 Linux。")
	}
	fd, err := unix.Openat2(root, name, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(mode), Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return -1, fmt.Errorf("openat2 %s（需要 Linux 5.6+）：%w", name, err)
	}
	return fd, nil
}
func ensureParent(root int, destination string, beforeMkdir func(int) error) error {
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(path.Dir(destination), "/"), "/") {
		if part == "" {
			continue
		}
		current += "/" + part
		fd, err := confinedOpen(root, current, unix.O_PATH|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) {
			parent, e := confinedOpen(root, path.Dir(current), unix.O_PATH|unix.O_DIRECTORY, 0)
			if e != nil {
				return e
			}
			if beforeMkdir != nil {
				e = beforeMkdir(parent)
			}
			if e == nil {
				e = unix.Mkdirat(parent, part, 0755)
				if errors.Is(e, unix.EEXIST) {
					e = nil
				}
			}
			unix.Close(parent)
			if e != nil {
				return e
			}
			fd, err = confinedOpen(root, current, unix.O_PATH|unix.O_DIRECTORY, 0)
		}
		if err != nil {
			return err
		}
		unix.Close(fd)
	}
	return nil
}
func fdMountID(fd int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("self/fdinfo/%d", fd))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "mnt_id:") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				return fields[1], nil
			}
		}
	}
	return "", errors.New("无法确定目标挂载点。")
}
func rejectSharedMount(fd int, mountinfo string) error {
	id, err := fdMountID(fd)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return err
	}
	return checkMountInfo(id, string(data))
}
func checkMountInfo(id, text string) error {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[0] != id {
			continue
		}
		for _, field := range fields[6:] {
			if field == "-" {
				return nil
			}
			if strings.HasPrefix(field, "shared:") {
				return errors.New("目标路径处于 shared 挂载中，拒绝向其他命名空间传播挂载。")
			}
		}
		return errors.New("无效的 mountinfo 记录")
	}
	return errors.New("目标路径不在容器的挂载命名空间内。")
}

// Called only in a disposable worker. LockOSThread and CLONE_FS are essential:
// Go has multiple runtime threads, and setns refuses a shared filesystem context.
// Never unlock this thread after entering the container's mount namespace.
func attachSocket(pid int, source, destination string, owner uint32, out io.Writer) error {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return errors.New("热挂载目前支持 x86_64/aarch64 Linux。")
	}
	runtime.LockOSThread()
	proc, err := unix.Open("/proc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(proc)
	process, err := unix.Openat(proc, strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(process)
	ns, err := unix.Openat(process, "ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(ns)
	root, err := unix.Openat(process, "root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	src, err := unix.Open(source, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(src)
	var sourceStat, nsStat, selfStat unix.Stat_t
	if err = unix.Fstat(src, &sourceStat); err != nil {
		return err
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFSOCK || sourceStat.Uid != owner {
		return errors.New("源必须是 docker-rootless 拥有的 Unix socket。")
	}
	if err = unix.Fstat(ns, &nsStat); err != nil {
		return err
	}
	if err = unix.Stat("/proc/self/ns/mnt", &selfStat); err != nil {
		return err
	}
	if nsStat.Ino == selfStat.Ino {
		return errors.New("目标与管理工具处于同一挂载命名空间，拒绝操作。")
	}
	tree, err := unix.OpenTree(src, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("open_tree socket: %w", err)
	}
	defer unix.Close(tree)
	if err = unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare filesystem context: %w", err)
	}
	if err = unix.Setns(ns, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns: %w", err)
	}
	if err = unix.Fchdir(proc); err != nil {
		return err
	}
	mountinfo := fmt.Sprintf("%d/mountinfo", pid)
	if err = ensureParent(root, destination, func(fd int) error { return rejectSharedMount(fd, mountinfo) }); err != nil {
		return err
	}
	parent, err := confinedOpen(root, path.Dir(destination), unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	err = rejectSharedMount(parent, mountinfo)
	unix.Close(parent)
	if err != nil {
		return err
	}
	target, err := confinedOpen(root, destination, unix.O_PATH, 0)
	if errors.Is(err, unix.ENOENT) {
		created, e := confinedOpen(root, destination, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		unix.Close(created)
		target, err = confinedOpen(root, destination, unix.O_PATH, 0)
	}
	if err != nil {
		return err
	}
	defer func() {
		if target >= 0 {
			unix.Close(target)
		}
	}()
	var info unix.Stat_t
	if err = unix.Fstat(target, &info); err != nil {
		return err
	}
	for info.Mode&unix.S_IFMT == unix.S_IFSOCK && info.Nlink == 0 {
		if err = rejectSharedMount(target, mountinfo); err != nil {
			return err
		}
		if err = unix.Unmount(fmt.Sprintf("self/fd/%d", target), unix.MNT_DETACH); err != nil {
			return fmt.Errorf("detach stale socket mount: %w", err)
		}
		unix.Close(target)
		target = -1
		target, err = confinedOpen(root, destination, unix.O_PATH, 0)
		if err != nil {
			return err
		}
		if err = unix.Fstat(target, &info); err != nil {
			return err
		}
	}
	if info.Mode&unix.S_IFMT != unix.S_IFSOCK && !(info.Mode&unix.S_IFMT == unix.S_IFREG && info.Size == 0) {
		return fmt.Errorf("目标不是 socket 或空文件，拒绝覆盖：%s", destination)
	}
	if info.Dev == sourceStat.Dev && info.Ino == sourceStat.Ino {
		fmt.Fprintln(out, "该容器已挂载当前 socket，无需重复挂载。")
		return nil
	}
	if err = rejectSharedMount(target, mountinfo); err != nil {
		return err
	}
	if err = unix.MoveMount(tree, "", target, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount socket: %w", err)
	}
	verify, err := confinedOpen(root, destination, unix.O_PATH, 0)
	if err != nil {
		return err
	}
	defer unix.Close(verify)
	var mounted unix.Stat_t
	if err = unix.Fstat(verify, &mounted); err != nil {
		return err
	}
	if mounted.Dev != sourceStat.Dev || mounted.Ino != sourceStat.Ino {
		return errors.New("容器路径在挂载时发生变化，请重新执行 add。")
	}
	return nil
}
