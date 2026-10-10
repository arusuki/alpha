package rootless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const runtimeDirectory = "/run/rootless-docker"
const stateDirectory = "/var/lib/rootless-docker"
const supervisorDirectory = "/usr/local/libexec/rootless-docker"
const supervisorPath = supervisorDirectory + "/supervisor"

// Enter the host filesystem and network on one disposable OS thread, then exec
// a fresh Go runtime. Keep the container cgroup: only the user service belongs
// to host systemd. No mounts are added to the host namespace here.
func enterHost(args []string) error {
	mapping, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return err
	}
	if strings.Join(strings.Fields(string(mapping)), " ") != "0 0 4294967295" {
		return errors.New("管理容器必须使用宿主机 user namespace（不支持 userns-remap/rootless 外层 Docker）")
	}
	runtime.LockOSThread()
	root, err := unix.Open("/proc/1/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err = unix.Unshare(unix.CLONE_FS); err != nil {
		return err
	}
	for _, name := range []string{"mnt", "net"} {
		fd, e := unix.Open("/proc/1/ns/"+name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		e = unix.Setns(fd, 0)
		unix.Close(fd)
		if e != nil {
			return fmt.Errorf("进入宿主机 %s namespace：%w", name, e)
		}
	}
	if err = unix.Fchdir(root); err != nil {
		return err
	}
	if err = unix.Chroot("."); err != nil {
		return err
	}
	if err = unix.Chdir("/"); err != nil {
		return err
	}
	clean := []string{"rootless-docker"}
	for _, arg := range args {
		if arg != "--host-namespaces" {
			clean = append(clean, arg)
		}
	}
	return unix.Exec("/proc/self/exe", clean, os.Environ())
}

func peerCredentials(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) { cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil {
		return nil, err
	}
	return cred, sockErr
}

// This process is started by the host user's systemd manager, outside the
// privileged container's cgroup. EOF from the owning daemon is a stop event,
// including docker kill/OOM: it requires no timer or health checker.
func supervise(leasePath, launch string) error {
	if os.Geteuid() == 0 {
		return errors.New("rootless supervisor 必须以非 root 用户运行")
	}
	c, err := net.DialTimeout("unix", leasePath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("管理 daemon 不可用，拒绝启动 dockerd：%w", err)
	}
	defer c.Close()
	cred, err := peerCredentials(c.(*net.UnixConn))
	if err != nil {
		return err
	}
	if cred.Uid != 0 {
		return errors.New("生命周期连接的 daemon 必须属于 host root")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	cmd := exec.Command(launch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return superviseProcess(ctx, c, cmd, 75*time.Second)
}

func superviseProcess(ctx context.Context, lease io.Reader, cmd *exec.Cmd, grace time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	lost := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, lease); close(lost) }()
	select {
	case err := <-done:
		return err
	case <-lost:
	case <-ctx.Done():
	}
	// RootlessKit forwards termination to dockerd; systemd KillMode=mixed
	// additionally reaps the service cgroup when this supervisor exits.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return errors.New("rootless dockerd 未及时停止，已终止进程组")
	}
}

type leaseServer struct {
	listener *net.UnixListener
	mu       sync.Mutex
	clients  map[*net.UnixConn]struct{}
	closed   bool
	wg       sync.WaitGroup
}

func startLeaseServer(path string, uid uint32) (*leaseServer, error) {
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chown(path, int(uid), -1); err == nil {
		err = os.Chmod(path, 0600)
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	s := &leaseServer{listener: l, clients: map[*net.UnixConn]struct{}{}}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			cred, err := peerCredentials(c)
			if err != nil || cred.Uid != uid {
				c.Close()
				continue
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				c.Close()
				return
			}
			s.clients[c] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
			go func() {
				defer s.wg.Done()
				_, _ = io.Copy(io.Discard, c)
				s.mu.Lock()
				delete(s.clients, c)
				s.mu.Unlock()
				c.Close()
			}()
		}
	}()
	return s, nil
}
func (s *leaseServer) Close() {
	s.listener.Close()
	s.mu.Lock()
	s.closed = true
	for c := range s.clients {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// All persistent mount receipts are written under a root-owned directory, not
// in the rootless user's mutable home. Reject unsafe ancestors before writing.
func rootDirectory(path string, mode os.FileMode) error {
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, mode); err != nil {
				return err
			}
			st, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("管理目录及祖先必须属于 root 且不能由组/其他用户写入：%s", current)
		}
	}
	return os.Chmod(path, mode)
}
func removeStaleSocket(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("拒绝覆盖非 socket：%s", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("socket 已有服务监听：%s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(path)
}
func installSupervisor() error {
	if err := rootDirectory(supervisorDirectory, 0755); err != nil {
		return err
	}
	source, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer source.Close()
	f, err := os.CreateTemp(supervisorDirectory, ".supervisor-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = io.Copy(f, source); err == nil {
		err = f.Chmod(0755)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), supervisorPath)
}
