package rootless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const dramSource = "/run/dram-bw/control.sock"
const dramRuntime = "/run/dram-bw-manager"
const dramState = "/var/lib/dram-bw-manager"

func (d *daemon) source() string {
	if d.sourceSocket != "" {
		return d.sourceSocket
	}
	return layout(d.user).Socket
}
func (d *daemon) verifySource() error {
	if d.sourceSocket == "" {
		return d.manager.verifyDaemon(d.user)
	}
	st, err := socketIdentity(d.sourceSocket)
	if err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || st.Uid != d.user.UID {
		return errors.New("DRAM socket 类型或所属用户不正确")
	}
	return nil
}

// The DRAM image needs no Docker CLI or rootless engine. Only these read-only
// Docker API operations are used by the shared mount journal and event handler.
func dockerReadAPI(ctx context.Context, host string, transport *http.Transport, args ...string) (result, error) {
	if err := validateHost(host); err != nil {
		return result{}, err
	}
	endpoint, kind := "", ""
	switch {
	case len(args) == 3 && args[0] == "info":
		endpoint = "/info"
	case len(args) >= 3 && args[0] == "container" && args[1] == "inspect":
		id := args[len(args)-1]
		if !validContainerName(id) {
			return result{}, errors.New("容器 ID 无效")
		}
		endpoint, kind = "/containers/"+url.PathEscape(id)+"/json", "inspect"
	case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
		endpoint, kind = "/containers/json?all=1", "list"
	default:
		return result{}, errors.New("不支持的 DRAM Docker 查询")
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+endpoint, nil)
	if err != nil {
		return result{}, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return result{}, err
	}
	if resp.StatusCode != 200 {
		return result{}, fmt.Errorf("Docker 查询失败（%d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if kind == "inspect" {
		raw = append(append([]byte{'['}, raw...), ']')
	}
	if kind == "list" {
		var items []struct{ ID string }
		if err = json.Unmarshal(raw, &items); err != nil {
			return result{}, err
		}
		var ids []string
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		raw = []byte(strings.Join(ids, "\n"))
	}
	return result{Out: string(raw)}, nil
}

// Resolve absolute host symlinks (notably /var/run -> /run) inside the host
// root. A plain /proc/1/root prefix would follow them back into the container.
func hostUnixTransport(socket string) *http.Transport {
	return &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		root, err := unix.Open("/proc/1/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(root)
		fd, err := confinedOpen(root, socket, unix.O_PATH, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		return (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
	}}
}

// Observe source replacement without polling or starting a health-check timer.
// Watch before starting the collector so its first socket creation is included.
func watchDRAMSource(ctx context.Context, d *daemon) (func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if _, err = unix.InotifyAddWatch(fd, "/run/dram-bw", unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_ATTRIB|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "dram-source-events")
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		buf := make([]byte, 4096)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			d.mu.Lock()
			if ctx.Err() == nil {
				if err := d.reconcileSocket(); err != nil {
					fmt.Fprintln(os.Stderr, "DRAM socket 恢复挂载：", err)
				}
			}
			d.mu.Unlock()
		}
	}()
	return func() { _ = f.Close() }, nil
}

// A separate privileged service in the DRAM container; it neither starts nor
// connects to rootless dockerd. Only the sampling socket reaches user containers.
func runDRAMDaemon(args []string) (retErr error) {
	if os.Geteuid() != 0 || os.Getpid() == 1 {
		return errors.New("DRAM 挂载管理需要 privileged + pid:host + userns:host 容器")
	}
	mapping, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return err
	}
	if strings.Join(strings.Fields(string(mapping)), " ") != "0 0 4294967295" {
		return errors.New("DRAM 管理容器需要宿主机 user namespace")
	}
	var own, init syscall.Stat_t
	if err = syscall.Stat("/proc/self/ns/pid", &own); err != nil {
		return err
	}
	if err = syscall.Stat("/proc/1/ns/pid", &init); err != nil {
		return err
	}
	if own.Ino != init.Ino {
		return errors.New("DRAM 管理容器需要 pid:host")
	}
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{{dramRuntime, 0755}, {dramState, 0700}, {"/run/dram-bw", 0755}} {
		if err := rootDirectory(directory.path, directory.mode); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(dramRuntime+"/daemon.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("已有 DRAM 管理服务运行")
	}
	store, err := loadBindings(dramState + "/bindings.json")
	if err != nil {
		return err
	}
	boot, err := hostBootID()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	m := newManager()
	m.ctx = ctx
	m.dockerAPI = func(host string, args ...string) (result, error) {
		return dockerReadAPI(m.ctx, host, hostUnixTransport(strings.TrimPrefix(host, "unix://")), args...)
	}
	d := &daemon{manager: m, user: account{UID: 0}, sourceSocket: dramSource, ctx: ctx, store: store, bootID: boot, stateFile: dramState + "/bindings.json"}
	d.savedBindings, err = jsonBytes(store)
	if err != nil {
		return err
	}
	closeWatch, err := watchDRAMSource(ctx, d)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		closeWatch()
		d.wg.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		m.ctx = context.Background()
		retErr = errors.Join(retErr, d.detachAll())
	}()
	socket := dramRuntime + "/control.sock"
	if err = removeStaleSocket(socket); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chown(socket, 0, os.Getegid()); err != nil {
		return err
	}
	if err = os.Chmod(socket, 0660); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(controlPath, d.control)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	defer server.Close()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	// Enforce a single known source even when original Compose arguments override it.
	for i, arg := range args {
		if arg == "--socket" && (i+1 == len(args) || args[i+1] != dramSource) || strings.HasPrefix(arg, "--socket=") && arg != "--socket="+dramSource {
			return errors.New("DRAM 授权挂载要求采集 socket 为 /run/dram-bw/control.sock")
		}
	}
	child := exec.Command("/usr/local/bin/dram-bwd", append([]string{"--settings", "/run/dram-bw/service-settings"}, args...)...)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = child.Start(); err != nil {
		return err
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	d.mu.Lock()
	d.syncWatchers()
	d.mu.Unlock()
	select {
	case err = <-childDone:
		cancel()
		if err == nil {
			return errors.New("DRAM 采集进程已退出")
		}
		return err
	case err = <-serverDone:
		retErr = err
	case <-ctx.Done():
	}
	cancel()
	_ = server.Close()
	_ = child.Process.Signal(syscall.SIGTERM)
	select {
	case <-childDone:
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		<-childDone
	}
	return retErr
}
