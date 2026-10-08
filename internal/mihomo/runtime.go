package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type process struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}
type Manager struct {
	mu          sync.Mutex
	store       Store
	state       runtimeState
	dir, socket string
	client      *http.Client
	process     *process
	error       string
	closed      bool
}
type Status struct {
	Running    bool    `json:"running"`
	Enabled    bool    `json:"enabled"`
	Ready      bool    `json:"ready"`
	PID        int     `json:"pid"`
	Error      string  `json:"error"`
	Digest     string  `json:"digest"`
	ProxyCount int     `json:"proxy_count"`
	Groups     []Group `json:"groups"`
}

// New must be called after the owning service acquires its existing data-dir lock.
func New(db *platform.Database) (*Manager, error) {
	s := Store{db}
	state, err := s.runtime()
	if err != nil {
		return nil, err
	}
	m := &Manager{store: s, state: state, dir: filepath.Join(db.Directory, "mihomo")}
	if err = os.MkdirAll(m.dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(m.dir, 0700); err != nil {
		return nil, err
	}
	m.socket = filepath.Join(m.dir, "controller.sock")
	m.client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", m.socket)
	}}}
	if state.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err = m.start(ctx); err != nil {
			m.error = err.Error()
		}
		cancel()
	}
	return m, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.stop()
	m.client.CloseIdleConnections()
}
func (m *Manager) alive() bool {
	if m.process == nil {
		return false
	}
	select {
	case <-m.process.done:
		return false
	default:
		return true
	}
}
func (m *Manager) stop() {
	p := m.process
	if p == nil {
		return
	}
	if m.alive() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
	m.process = nil
	_ = os.Remove(m.socket)
}
func (m *Manager) managed(b Bundle) ([]byte, error) {
	if len(m.socket) > 100 {
		return nil, fmt.Errorf("代理数据目录过长，Unix socket 路径须不超过 100 字节")
	}
	c, _, _, err := inspectConfig([]byte(b.YAML))
	if err != nil {
		return nil, err
	}
	for k := range c {
		if strings.HasPrefix(k, "external-") || k == "secret" {
			delete(c, k)
		}
	}
	c["external-controller-unix"] = m.socket
	return yaml.Marshal(c)
}
func privateWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func binaryPath(binary string) (string, error) {
	if binary != "mihomo" && !filepath.IsAbs(binary) {
		return "", fmt.Errorf("mihomo 路径无效")
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("未找到可执行的 mihomo：请在该节点安装核心或设置绝对路径")
	}
	return path, nil
}
func (m *Manager) validate(ctx context.Context, b Bundle) ([]byte, error) {
	if b.Digest != bundleDigest(b) {
		return nil, fmt.Errorf("代理配置摘要不匹配")
	}
	raw, err := m.managed(b)
	if err != nil {
		return nil, err
	}
	path, err := binaryPath(b.Binary)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(m.dir, ".validate-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-t", "-d", m.dir, "-f", f.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	// Core diagnostics can contain subscription credentials; do not persist or
	// return them. Template/structure errors are reported before this point.
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("mihomo -t 配置校验失败（%v）；请检查协议参数、规则文件及核心版本，已保留原配置", err)
	}
	return raw, nil
}
func (m *Manager) start(ctx context.Context) error {
	if m.alive() {
		return nil
	}
	if m.state.Bundle.Digest == "" {
		return fmt.Errorf("尚无已应用的代理配置，请先保存并等待配置下发")
	}
	raw, err := m.validate(ctx, m.state.Bundle)
	if err != nil {
		return err
	}
	path, err := binaryPath(m.state.Bundle.Binary)
	if err != nil {
		return err
	}
	file := filepath.Join(m.dir, "config.yaml")
	if err = privateWrite(file, raw); err != nil {
		return err
	}
	_ = os.Remove(m.socket)
	cmd := exec.Command(path, "-d", m.dir, "-f", file)
	cmd.Dir = m.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("启动 mihomo 失败：%w", err)
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	m.process = p
	go func() { p.err = cmd.Wait(); close(p.done) }()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !m.alive() {
			return fmt.Errorf("mihomo 启动后退出：%v；请检查端口占用和运行权限", p.err)
		}
		if err = m.api(ctx, "GET", "/version", nil, nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			m.stop()
			return fmt.Errorf("mihomo 控制接口未就绪，请检查端口占用和运行权限")
		case <-ticker.C:
		}
	}
	if err = m.restoreSelections(ctx, m.state); err != nil {
		m.stop()
		return err
	}
	m.error = ""
	return nil
}
func (m *Manager) api(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://mihomo"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("mihomo 控制接口不可用")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mihomo 控制接口返回 HTTP %d", resp.StatusCode)
	}
	if out != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxConfig+1))
		if err != nil || len(raw) > MaxConfig {
			return fmt.Errorf("mihomo 响应超出限制")
		}
		if json.Unmarshal(raw, out) != nil {
			return fmt.Errorf("mihomo 响应格式无效")
		}
	}
	return nil
}
func (m *Manager) reload(ctx context.Context, b Bundle) error {
	raw, err := m.managed(b)
	if err != nil {
		return err
	}
	return m.api(ctx, "PUT", "/configs?force=true", map[string]string{"path": "", "payload": string(raw)}, nil)
}
func (m *Manager) restoreSelections(ctx context.Context, s runtimeState) error {
	for _, g := range s.Bundle.Groups {
		if n := s.Selections[g.Name]; n != "" && g.Type == "select" && slices.Contains(g.Proxies, n) {
			if err := m.api(ctx, "PUT", "/proxies/"+url.PathEscape(g.Name), map[string]string{"name": n}, nil); err != nil {
				return fmt.Errorf("恢复策略组 %s 选择失败：%w", g.Name, err)
			}
		}
	}
	return nil
}

func (m *Manager) Apply(ctx context.Context, b Bundle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("代理服务正在关闭")
	}
	if b.Digest == "" || b.Digest != bundleDigest(b) {
		return fmt.Errorf("代理配置摘要不匹配或为空")
	}
	if b.Digest == m.state.Bundle.Digest {
		return nil
	}
	_, groups, count, err := inspectConfig([]byte(b.YAML))
	if err != nil {
		return err
	}
	b.Groups = groups
	b.ProxyCount = count
	raw, err := m.validate(ctx, b)
	if err != nil {
		m.error = err.Error()
		return err
	}
	old := m.state
	next := runtimeState{Bundle: b, Enabled: old.Enabled, Selections: map[string]string{}}
	for _, g := range b.Groups {
		if n := old.Selections[g.Name]; g.Type == "select" && slices.Contains(g.Proxies, n) {
			next.Selections[g.Name] = n
		}
	}
	running := m.alive()
	if running && old.Bundle.Binary != b.Binary {
		return fmt.Errorf("更换核心路径前请先停止该节点代理")
	}
	configPath := filepath.Join(m.dir, "config.yaml")
	if err = privateWrite(configPath, raw); err != nil {
		return err
	}
	rollback := func(cause error) error {
		var fileErr error
		if old.Bundle.Digest != "" {
			previous, e := m.managed(old.Bundle)
			fileErr = e
			if e == nil {
				fileErr = privateWrite(configPath, previous)
			}
		} else {
			fileErr = os.Remove(configPath)
		}
		if running {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if e := m.reload(rollbackCtx, old.Bundle); e == nil {
				e = m.restoreSelections(rollbackCtx, old)
				if e != nil {
					m.stop()
					return fmt.Errorf("%v；恢复旧选择失败，服务已停止：%w", cause, e)
				}
			} else {
				m.stop()
				return fmt.Errorf("%v；恢复旧配置失败，服务已停止：%w", cause, e)
			}
		}
		if fileErr != nil {
			return fmt.Errorf("%v；恢复配置文件失败：%w", cause, fileErr)
		}
		return cause
	}
	if running {
		if err = m.reload(ctx, b); err == nil {
			err = m.restoreSelections(ctx, next)
		}
		if err != nil {
			err = rollback(err)
			m.error = err.Error()
			return err
		}
	}
	if err = m.store.saveRuntime(next, "control", "mihomo.apply"); err != nil {
		err = rollback(err)
		m.error = err.Error()
		return err
	}
	m.state = next
	m.error = ""
	return nil
}
func (m *Manager) Service(ctx context.Context, action, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("代理服务正在关闭")
	}
	if action != "start" && action != "stop" && action != "restart" {
		return httpapi.NewError(400, "服务操作需为 start、stop 或 restart")
	}
	next := m.state
	next.Enabled = action != "stop"
	if next.Enabled && next.Bundle.Digest == "" {
		return httpapi.NewError(409, "尚无已应用的配置，请先保存并等待下发")
	}
	if err := m.store.saveRuntime(next, actor, "mihomo."+action); err != nil {
		return err
	}
	m.state = next
	if action == "stop" || action == "restart" {
		m.stop()
	}
	if action != "stop" {
		if err := m.start(ctx); err != nil {
			m.error = err.Error()
			return err
		}
	}
	m.error = ""
	return nil
}
func (m *Manager) Select(ctx context.Context, group, name, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("代理服务正在关闭")
	}
	valid := false
	for _, g := range m.state.Bundle.Groups {
		if g.Name == group && g.Type == "select" && slices.Contains(g.Proxies, name) {
			valid = true
			break
		}
	}
	if !valid {
		return httpapi.NewError(400, "只能选择模板中 select 策略组的候选节点")
	}
	next := m.state
	next.Selections = map[string]string{}
	for k, v := range m.state.Selections {
		next.Selections[k] = v
	}
	next.Selections[group] = name
	var previous string
	if m.alive() {
		var info struct {
			Now string `json:"now"`
		}
		if err := m.api(ctx, "GET", "/proxies/"+url.PathEscape(group), nil, &info); err != nil {
			return err
		}
		previous = info.Now
		if err := m.api(ctx, "PUT", "/proxies/"+url.PathEscape(group), map[string]string{"name": name}, nil); err != nil {
			return err
		}
	}
	if err := m.store.saveRuntime(next, actor, "mihomo.select"); err != nil {
		if previous != "" {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e := m.api(rollbackCtx, "PUT", "/proxies/"+url.PathEscape(group), map[string]string{"name": previous}, nil); e != nil {
				m.error = "保存选择失败且无法恢复运行选择"
			}
		}
		return err
	}
	m.state = next
	return nil
}
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Running: m.alive(), Enabled: m.state.Enabled, Error: m.error, Digest: m.state.Bundle.Digest, ProxyCount: m.state.Bundle.ProxyCount, Groups: []Group{}}
	for _, g := range m.state.Bundle.Groups {
		copy := g
		copy.Proxies = slices.Clone(g.Proxies)
		copy.Now = m.state.Selections[g.Name]
		s.Groups = append(s.Groups, copy)
	}
	if !s.Running {
		if m.process != nil && m.state.Enabled && s.Error == "" {
			s.Error = fmt.Sprintf("mihomo 已退出：%v", m.process.err)
		}
		return s
	}
	s.PID = m.process.cmd.Process.Pid
	var live struct {
		Proxies map[string]struct {
			Now string   `json:"now"`
			All []string `json:"all"`
		} `json:"proxies"`
	}
	if err := m.api(ctx, "GET", "/proxies", nil, &live); err != nil {
		s.Error = err.Error()
		return s
	}
	s.Ready = true
	for i, g := range s.Groups {
		v, ok := live.Proxies[g.Name]
		if !ok {
			continue
		}
		s.Groups[i].Now = v.Now
		n := v.Now
		seen := map[string]bool{}
		for n != "" && !seen[n] {
			seen[n] = true
			next := live.Proxies[n].Now
			if next == "" {
				break
			}
			n = next
		}
		s.Groups[i].Resolved = n
	}
	return s
}

func (m *Manager) Dispatch(w http.ResponseWriter, r *http.Request) (int, any, error) {
	switch {
	case r.URL.Path == Path+"/status" && r.Method == "GET":
		return 200, m.Status(r.Context()), nil
	case r.URL.Path == Path+"/apply" && r.Method == "PUT":
		var b Bundle
		if err := decode(w, r, &b); err != nil {
			return 0, nil, err
		}
		if err := m.Apply(r.Context(), b); err != nil {
			return 0, nil, httpapi.NewError(422, err.Error())
		}
		return 200, map[string]bool{"ok": true}, nil
	case r.URL.Path == Path+"/service" && r.Method == "POST":
		var v struct {
			Action string `json:"action"`
		}
		if err := decode(w, r, &v); err != nil {
			return 0, nil, err
		}
		if err := m.Service(r.Context(), v.Action, "control"); err != nil {
			return 0, nil, httpapi.NewError(409, err.Error())
		}
		return 200, m.Status(r.Context()), nil
	case r.URL.Path == Path+"/selection" && r.Method == "PUT":
		var v struct {
			Group string `json:"group"`
			Name  string `json:"name"`
		}
		if err := decode(w, r, &v); err != nil {
			return 0, nil, err
		}
		if err := m.Select(r.Context(), v.Group, v.Name, "control"); err != nil {
			return 0, nil, httpapi.NewError(409, err.Error())
		}
		return 200, m.Status(r.Context()), nil
	}
	return 0, nil, httpapi.NewError(404, "代理管理接口不存在")
}
func decode(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return httpapi.NewError(400, "代理请求 JSON 无效或超过 4 MiB")
	}
	if d.Decode(new(any)) != io.EOF {
		return httpapi.NewError(400, "代理请求必须为单个 JSON 对象")
	}
	return nil
}
