package rootless

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const controlPath = "/_rootless/control"

type controlRequest struct {
	Args []string `json:"args"`
}
type controlResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}
type daemon struct {
	manager           *manager
	user              account
	mu                sync.Mutex
	store             bindingStore
	stateFile, bootID string
	ctx               context.Context
	watchers          map[string]*eventWatcher
	savedBindings     []byte
	wg                sync.WaitGroup
}

func runDaemon(o options) (retErr error) {
	if filepath.Clean(o.ControlSocket) != runtimeDirectory+"/control.sock" {
		return errors.New("daemon 管理 socket 固定为 /run/rootless-docker/control.sock；客户端可通过挂载目录使用其他路径")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := rootDirectory(runtimeDirectory, 0755); err != nil {
		return err
	}
	if err := rootDirectory(stateDirectory, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(runtimeDirectory+"/daemon.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("已有 rootless 管理 daemon 运行")
	}
	store, err := loadBindings(stateDirectory + "/bindings.json")
	if err != nil {
		return err
	}
	boot, err := hostBootID()
	if err != nil {
		return err
	}
	if err = installSupervisor(); err != nil {
		return err
	}
	m := newManager()
	m.ctx = ctx
	m.run = func(args, env []string, timeout time.Duration, check bool) (result, error) {
		return runCommandContext(ctx, args, env, timeout, check)
	}
	if err = m.initialize(o); err != nil {
		return err
	}
	u, err := lookupAccount()
	if err != nil {
		return err
	}
	lease, err := startLeaseServer(runtimeDirectory+"/lease.sock", u.UID)
	if err != nil {
		return err
	}
	d := &daemon{manager: m, user: u, store: store, bootID: boot, stateFile: stateDirectory + "/bindings.json", ctx: ctx}
	d.savedBindings, err = jsonBytes(store)
	if err != nil {
		lease.Close()
		return err
	}
	// Register cleanup before starting: partially successful startup must not
	// leave a detached dockerd behind. Independent cleanup also covers errors.
	defer func() {
		cancel()
		d.wg.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		m.ctx, m.run = context.Background(), runCommand
		if e := d.detachAll(); e != nil {
			retErr = errors.Join(retErr, fmt.Errorf("清理挂载：%w", e))
		}
		lease.Close()
		if _, e := m.systemctl(u, "stop", serviceName); e != nil {
			retErr = errors.Join(retErr, fmt.Errorf("停止 dockerd：%w", e))
		}
	}()
	if _, err = m.systemctl(u, "start", serviceName); err != nil {
		return err
	}
	if err = m.verifyDaemon(u); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = removeStaleSocket(o.ControlSocket); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: o.ControlSocket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chown(o.ControlSocket, 0, o.SocketGID); err == nil {
		err = os.Chmod(o.ControlSocket, 0660)
	}
	if err != nil {
		return err
	}
	server := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	d.mu.Lock()
	d.syncWatchers()
	d.mu.Unlock()
	fmt.Fprintf(os.Stdout, "rootless 管理服务已就绪：%s\nDockerd：%s\n", o.ControlSocket, layout(u).Socket)
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			retErr = err
		}
	}
	cancel()
	// Close active HTTP requests and streaming logs; serialize against any
	// current mutation before stopping the host service.
	_ = server.Close()
	return retErr
}

func unixTransport(socket string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableCompression: true}
}
func (d *daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(controlPath, d.control)
	mux.HandleFunc("/_rootless/logs", d.logs)
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = unixTransport(layout(d.user).Socket)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "rootless dockerd 不可用："+err.Error(), http.StatusBadGateway)
	}
	mux.Handle("/", proxy)
	return mux
}
func (d *daemon) control(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "需要 POST", http.StatusMethodNotAllowed)
		return
	}
	var request controlRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
		return
	}
	o, err := parseOptions(request.Args)
	if err == nil && o.Help {
		err = errors.New("请在客户端查看 --help")
	}
	if err == nil {
		switch o.Command {
		case "add", "remove", "list", "bindings", "status", "proxy", "restart":
		case "test":
			if len(o.Args) == 0 || o.Args[0] == "exec" {
				err = errors.New("交互命令必须在客户端执行")
			}
		default:
			err = errors.New("该命令不允许通过管理 API 执行")
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		http.Error(w, "管理服务正在停止", http.StatusServiceUnavailable)
		return
	}
	var output bytes.Buffer
	originalOut, originalErr := d.manager.out, d.manager.errOut
	d.manager.out, d.manager.errOut = &output, &output
	defer func() { d.manager.out, d.manager.errOut = originalOut, originalErr }()
	switch o.Command {
	case "add":
		err = d.add(o)
		d.syncWatchers()
	case "remove":
		err = d.remove(o)
		d.syncWatchers()
	case "list":
		err = json.NewEncoder(&output).Encode(d.store.Bindings)
	case "bindings":
		err = json.NewEncoder(&output).Encode(d.bindingStatuses())
	case "status":
		err = d.manager.verifyDaemon(d.user)
		if err == nil {
			fmt.Fprintf(&output, "管理服务运行中；rootless dockerd 已验证。\n数据：%s\n关联：%d\n", layout(d.user).Data, len(d.store.Bindings))
		}
	case "proxy":
		err = d.manager.configureProxy(d.user, o)
		if err == nil && (len(o.Proxies) > 0 || o.Clear || o.AllowLoopback != nil) {
			err = d.reconcile("", "")
		}
	case "restart":
		_, err = d.manager.systemctl(d.user, "restart", serviceName)
		if err == nil {
			err = d.manager.verifyDaemon(d.user)
		}
		if err == nil {
			err = d.reconcile("", "")
		}
		if err == nil {
			fmt.Fprintln(&output, "rootless dockerd 已重启并恢复挂载。")
		}
	case "test":
		err = d.testContainer(o.Args)
		d.syncWatchers()
	}
	response := controlResponse{Output: output.String()}
	if err != nil {
		response.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
func (d *daemon) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "需要 GET", 405)
		return
	}
	cmd := exec.CommandContext(r.Context(), "tail", "-n", "100", "-F", layout(d.user).Log+"/dockerd.log")
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err = cmd.Start(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	_ = http.NewResponseController(w).Flush()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if _, err = fmt.Fprintln(w, scanner.Text()); err != nil {
			return
		}
		if err = http.NewResponseController(w).Flush(); err != nil {
			return
		}
	}
}

func clientRequest(o options, args []string) (string, error) {
	transport := unixTransport(o.ControlSocket)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute}
	data, err := json.Marshal(controlRequest{Args: args})
	if err != nil {
		return "", err
	}
	response, err := client.Post("http://rootless"+controlPath, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("连接管理 daemon（%s）：%w", o.ControlSocket, err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("管理服务：%s", strings.TrimSpace(string(b)))
	}
	var result controlResponse
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return result.Output, errors.New(result.Error)
	}
	return result.Output, nil
}
func clientDockerEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_TLS", "DOCKER_CERT_PATH", "DOCKER_API_VERSION":
			continue
		}
		env = append(env, entry)
	}
	return env
}
func runClient(o options, args []string) error {
	if o.Command == "docker" {
		return execCommand(append([]string{"docker", "--host", "unix://" + o.ControlSocket}, o.Args...), clientDockerEnv())
	}
	if o.Command == "logs" {
		transport := unixTransport(o.ControlSocket)
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		r, err := client.Get("http://rootless/_rootless/logs")
		if err != nil {
			return err
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("日志请求失败：%s", r.Status)
		}
		_, err = io.Copy(os.Stdout, r.Body)
		return err
	}
	if o.Command == "test" && o.Args[0] == "exec" {
		name := "rootless-cli-test"
		if len(o.Args) == 2 {
			name = o.Args[1]
		}
		out, err := clientRequest(o, []string{"test", "up", name})
		fmt.Print(out)
		if err != nil {
			return err
		}
		cmd := []string{"docker", "--host", testHost, "exec", "-i"}
		if _, e := unix.IoctlGetTermios(0, unix.TCGETS); e == nil {
			cmd = append(cmd, "-t")
		}
		cmd = append(cmd, "--user", "0", name, "/bin/sh")
		return execCommand(cmd, clientDockerEnv())
	}
	out, err := clientRequest(o, args)
	fmt.Print(out)
	return err
}
