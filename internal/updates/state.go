// Package updates defines management protocol v1. Its health, settings and
// release messages remain stable independently of the business API protocols.
package updates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/updater"
)

const Path = "/api/management/v1"
const WebhookPath = "/api/webhooks/github"
const Protocol = 1

const githubTokenKeyFile = "update-credentials.key"

type Config struct {
	Command          string `json:"command"`
	Proxy            string `json:"http_proxy"`
	Repo             string `json:"repo"`
	Prerelease       bool   `json:"prerelease"`
	Automatic        bool   `json:"automatic"`
	WebhookSecret    string `json:"webhook_secret,omitempty"`
	HasSecret        bool   `json:"has_webhook_secret,omitempty"`
	ClearSecret      bool   `json:"clear_webhook_secret,omitempty"`
	GitHubToken      string `json:"github_token,omitempty"`
	HasGitHubToken   bool   `json:"has_github_token,omitempty"`
	ClearGitHubToken bool   `json:"clear_github_token,omitempty"`
}
type Release struct {
	Delivery   string    `json:"delivery"`
	Repo       string    `json:"repo"`
	Tag        string    `json:"tag"`
	Published  time.Time `json:"published_at"`
	Prerelease bool      `json:"prerelease"`
}
type state struct {
	GitHubTokenCiphertext string    `json:"github_token_ciphertext,omitempty"`
	DeliveryError         string    `json:"delivery_error,omitempty"`
	Format                int       `json:"format"`
	Revision              int       `json:"revision"`
	Config                Config    `json:"config"`
	Release               *Release  `json:"release,omitempty"`
	Forwarded             bool      `json:"forwarded"`
	Outbox                []Release `json:"outbox,omitempty"`
	Seen                  []string  `json:"seen,omitempty"`
	Attempted             string    `json:"attempted,omitempty"`
	Error                 string    `json:"error,omitempty"`
}
type Manager struct {
	mu                              sync.Mutex
	directory, role, id, executable string
	state                           state
	Requested                       chan struct{}
	plan                            *updater.ServicePlan
	ready, closed                   bool
	prepareCancel                   context.CancelFunc
	prepareDone                     chan struct{}
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func New(directory, role, id string) (*Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &Manager{directory: directory, role: role, id: id, executable: exe, Requested: make(chan struct{}, 1)}
	m.state = state{Format: 1, Revision: 1, Config: Config{Command: filepath.Join(filepath.Dir(exe), "alpha-updater"), Repo: "arusuki/alpha"}}
	raw, err := os.ReadFile(m.filename())
	if err == nil {
		var saved state
		if err = json.Unmarshal(raw, &saved); err != nil {
			return nil, fmt.Errorf("invalid update settings: %w; use a new data directory", err)
		}
		m.state = saved
		if m.state.Format != 1 || m.state.Revision < 1 {
			return nil, fmt.Errorf("unsupported update settings format; use a new data directory")
		}
		if m.state.Config.GitHubToken != "" {
			return nil, fmt.Errorf("unsupported plaintext GitHub token in update settings; use a new data directory")
		}
		if m.state.GitHubTokenCiphertext != "" {
			m.state.Config.GitHubToken, err = credentials.Decrypt(directory, githubTokenKeyFile, m.githubTokenPurpose(), m.state.GitHubTokenCiphertext)
			if err != nil {
				return nil, fmt.Errorf("decrypt update GitHub token: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err = m.state.Config.validate(role); err != nil {
		return nil, err
	}
	return m, nil
}
func (m *Manager) filename() string           { return filepath.Join(m.directory, "update-settings.json") }
func (m *Manager) githubTokenPurpose() string { return "updates.github-token/" + m.role + "/" + m.id }

func (m *Manager) verifyGitHubTokenKey() error {
	if m.state.GitHubTokenCiphertext == "" {
		return nil
	}
	token, err := credentials.Decrypt(m.directory, githubTokenKeyFile, m.githubTokenPurpose(), m.state.GitHubTokenCiphertext)
	if err != nil {
		return fmt.Errorf("decrypt update GitHub token: %w", err)
	}
	if token != m.state.Config.GitHubToken {
		return fmt.Errorf("update GitHub token does not match stored ciphertext")
	}
	return nil
}

func (m *Manager) commit(s state) error {
	// Verify the original key before any write, including a clear or rotation.
	// Never replace a missing key while there is still encrypted data.
	if err := m.verifyGitHubTokenKey(); err != nil {
		return err
	}
	if s.Config.GitHubToken == "" {
		s.GitHubTokenCiphertext = ""
	} else if s.Config.GitHubToken != m.state.Config.GitHubToken || m.state.GitHubTokenCiphertext == "" {
		encrypt := credentials.Encrypt
		if m.state.GitHubTokenCiphertext != "" {
			encrypt = credentials.EncryptExisting
		}
		var err error
		s.GitHubTokenCiphertext, err = encrypt(m.directory, githubTokenKeyFile, m.githubTokenPurpose(), s.Config.GitHubToken)
		if err != nil {
			return fmt.Errorf("encrypt update GitHub token: %w", err)
		}
	}
	persisted := s
	persisted.Config.GitHubToken = ""
	if err := updater.WriteJSON(m.filename(), persisted); err != nil {
		return err
	}
	m.state = s
	return nil
}
func (c Config) validate(role string) error {
	if !filepath.IsAbs(c.Command) || !repoPattern.MatchString(c.Repo) {
		return httpapi.NewError(400, "更新器必须是绝对路径，仓库必须为 owner/repo")
	}
	if c.Proxy != "" {
		u, e := url.Parse(c.Proxy)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return httpapi.NewError(400, "HTTP 代理必须为 http:// 或 https:// 地址")
		}
	}
	if c.WebhookSecret != "" && (role != "registry" || len(c.WebhookSecret) < 32 || len(c.WebhookSecret) > 256) {
		return httpapi.NewError(400, "只有 registry 可设置 webhook secret，长度须为 32–256 字节")
	}
	if len(c.GitHubToken) > 4096 || strings.IndexFunc(c.GitHubToken, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return httpapi.NewError(400, "GitHub token 必须为不含空白的 ASCII 字符，且不超过 4096 字节")
	}
	return nil
}
func (m *Manager) Settings() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.state.Config
	c.HasSecret = c.WebhookSecret != ""
	c.WebhookSecret = ""
	c.HasGitHubToken = c.GitHubToken != ""
	c.GitHubToken = ""
	return map[string]any{"revision": m.state.Revision, "config": c, "health": m.health(), "webhook_path": WebhookPath}
}
func (m *Manager) Save(revision int, c Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save(revision, c)
}
func (m *Manager) save(revision int, c Config) error {
	if m.plan != nil {
		return httpapi.NewError(409, "正在更新，请稍后修改设置")
	}
	if revision != m.state.Revision {
		return httpapi.NewError(409, "更新设置已改变，请重新载入")
	}
	if c.ClearSecret {
		c.WebhookSecret = ""
	} else if c.WebhookSecret == "" {
		c.WebhookSecret = m.state.Config.WebhookSecret
	}
	c.HasSecret = false
	c.ClearSecret = false
	if m.role != "control" && (c.GitHubToken != "" || c.ClearGitHubToken) {
		return httpapi.NewError(400, "GitHub Token 由总控统一配置，不能为节点独立设置")
	}
	if c.ClearGitHubToken {
		c.GitHubToken = ""
	} else if c.GitHubToken == "" {
		c.GitHubToken = m.state.Config.GitHubToken
	}
	c.HasGitHubToken = false
	c.ClearGitHubToken = false
	if err := c.validate(m.role); err != nil {
		return err
	}
	s := m.state
	s.Config = c
	s.Revision++
	if c.Repo != m.state.Config.Repo {
		s.Release = nil
		s.Outbox = nil
		s.Seen = nil
		s.Forwarded = false
		s.Attempted = ""
		s.Error = ""
	}
	return m.commit(s)
}

// GitHubToken returns the control's shared credential or a node's received copy.
// Only control may source the shared token from its service environment.
func (m *Manager) GitHubToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.githubToken()
}
func (m *Manager) githubToken() string {
	if m.state.Config.GitHubToken != "" || m.role != "control" {
		return m.state.Config.GitHubToken
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("GITHUB_TOKEN")
}
func (m *Manager) GitHubTokenSettings() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{"revision": m.state.Revision, "has_token": m.state.Config.GitHubToken != "", "has_environment_token": m.state.Config.GitHubToken == "" && m.githubToken() != ""}
}
func (m *Manager) SaveGitHubToken(revision int, token string, clear bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.role != "control" {
		return httpapi.NewError(403, "GitHub Token 只能在总控配置")
	}
	c := m.state.Config
	c.GitHubToken, c.ClearGitHubToken = token, clear
	return m.save(revision, c)
}

// ApplyGitHubToken is called only through the authenticated node management API.
// Repeated delivery does not rewrite settings or invalidate local form revisions.
func (m *Manager) ApplyGitHubToken(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.role == "control" {
		return httpapi.NewError(403, "总控不接受下发的 GitHub Token")
	}
	c := m.state.Config
	c.GitHubToken = token
	if err := c.validate(m.role); err != nil {
		return err
	}
	if token == m.state.Config.GitHubToken {
		return m.verifyGitHubTokenKey()
	}
	s := m.state
	s.Config = c
	return m.commit(s)
}
func (m *Manager) health() map[string]any {
	status := "idle"
	if m.plan != nil {
		status = "downloading"
		if m.ready {
			status = "restarting"
		}
	}
	var result updater.ServiceResult
	if m.plan == nil {
		if raw, err := os.ReadFile(filepath.Join(m.directory, "update-result.json")); err == nil {
			if json.Unmarshal(raw, &result) == nil {
				status = result.State
			}
		}
	}
	return map[string]any{"management_protocol": Protocol, "id": m.id, "mode": m.role, "version": buildinfo.Version, "healthy": true, "update_state": status, "release": m.state.Release, "error": m.state.Error, "result": result, "pending_deliveries": len(m.state.Outbox), "delivery_error": m.state.DeliveryError}
}
func (m *Manager) Health() map[string]any { m.mu.Lock(); defer m.mu.Unlock(); return m.health() }
func (m *Manager) Pending() *Release {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.role == "registry" {
		if len(m.state.Outbox) == 0 {
			return nil
		}
		r := m.state.Outbox[0]
		return &r
	}
	if m.state.Release == nil || m.state.Forwarded {
		return nil
	}
	r := *m.state.Release
	return &r
}
func (m *Manager) Forwarded(r Release) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	if m.role == "registry" {
		s.Outbox = append([]Release(nil), s.Outbox...)
		for i, event := range s.Outbox {
			if event.Delivery == r.Delivery {
				s.Outbox = append(s.Outbox[:i], s.Outbox[i+1:]...)
				s.DeliveryError = ""
				return m.commit(s)
			}
		}
		return nil
	}
	if s.Release != nil && s.Release.Delivery == r.Delivery {
		s.Forwarded = true
		return m.commit(s)
	}
	return nil
}
func (r Release) Validate() error {
	if !repoPattern.MatchString(r.Repo) || !deliveryPattern.MatchString(r.Delivery) || r.Published.IsZero() {
		return httpapi.NewError(400, "release 通知字段无效")
	}
	if err := updater.ValidateTag(r.Tag); err != nil {
		return httpapi.NewError(400, err.Error())
	}
	return nil
}

// Receive durably retains the latest publication, making duplicate and reordered
// deliveries harmless even across service restarts.
func (m *Manager) Receive(r Release) error {
	if err := r.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Repo != m.state.Config.Repo {
		return httpapi.NewError(409, "release 仓库与更新设置不一致")
	}
	if m.role == "registry" {
		for _, id := range m.state.Seen {
			if id == r.Delivery {
				return nil
			}
		}
		if len(m.state.Outbox) >= 128 {
			return httpapi.NewError(503, "release 通知队列已满，请恢复 control 连接后在 GitHub 重投")
		}
		s := m.state
		s.Outbox = append(append([]Release(nil), s.Outbox...), r)
		s.Seen = append(append([]string(nil), s.Seen...), r.Delivery)
		if len(s.Seen) > 256 {
			s.Seen = s.Seen[len(s.Seen)-256:]
		}
		if (!r.Prerelease || s.Config.Prerelease) && (s.Release == nil || r.Published.After(s.Release.Published)) {
			s.Release = &r
			s.Error = ""
		}
		return m.commit(s)
	}
	if r.Prerelease && !m.state.Config.Prerelease {
		return nil
	}
	if old := m.state.Release; old != nil && (old.Delivery == r.Delivery || !r.Published.After(old.Published)) {
		return nil
	}
	s := m.state
	s.Release = &r
	s.Forwarded = false
	s.Error = ""
	return m.commit(s)
}
func (m *Manager) Trigger(tag string) error { m.mu.Lock(); defer m.mu.Unlock(); return m.trigger(tag) }
func (m *Manager) trigger(tag string) error {
	if m.closed {
		return httpapi.NewError(503, "服务正在关闭")
	}
	if m.plan != nil {
		return httpapi.NewError(409, "更新已经排队")
	}
	if tag != "" {
		if err := updater.ValidateTag(tag); err != nil {
			return httpapi.NewError(400, err.Error())
		}
	}
	c := m.state.Config
	info, err := os.Lstat(c.Command)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return httpapi.NewError(400, "更新器不存在或不可执行，请检查绝对路径和服务账号权限")
	}
	if err := updater.ValidateTag(buildinfo.Version); err != nil {
		return httpapi.NewError(409, "当前程序没有受支持的 release 版本，不能自动更新")
	}
	if filepath.Base(m.executable) != "project-alpha" {
		return httpapi.NewError(409, "服务程序必须以 project-alpha 名称安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, e := exec.CommandContext(ctx, c.Command, "--service-protocol").Output()
	if e != nil || strings.TrimSpace(string(output)) != "2" {
		return httpapi.NewError(409, "更新器不支持后台下载协议 v2，请先安装新版 alpha-updater")
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(m.executable), ".alpha-update-pending")); !os.IsNotExist(e) {
		return httpapi.NewError(409, "上次更新需要人工恢复，请检查 .alpha-update-pending")
	}
	p := updater.ServicePlan{Format: 2, Command: c.Command, Executable: m.executable, Arguments: append([]string(nil), os.Args[1:]...), Directory: m.directory, Role: m.role, Repo: c.Repo, Proxy: c.Proxy, Prerelease: c.Prerelease, Tag: tag}
	if r := m.state.Release; r != nil && r.Tag == tag && r.Repo == c.Repo && (!r.Prerelease || c.Prerelease) {
		p.Published = true
	}
	if err := updater.WriteJSON(filepath.Join(m.directory, "update-service.json"), p); err != nil {
		return err
	}
	s := m.state
	s.Error = ""
	if err := m.commit(s); err != nil {
		return err
	}
	m.plan = &p
	prepareCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	m.prepareCancel = cancel
	m.prepareDone = make(chan struct{})
	go m.prepare(prepareCtx, cancel, m.prepareDone, p, m.githubToken())
	return nil
}
func (m *Manager) Automatic() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.state.Release
	if (m.role == "registry" && len(m.state.Outbox) > 0) || (m.role == "control" && !m.state.Forwarded) {
		return nil
	}
	if !m.state.Config.Automatic || r == nil || m.state.Attempted == r.Tag || m.plan != nil {
		return nil
	}
	if !updater.Newer(r.Tag, buildinfo.Version) {
		return nil
	}
	s := m.state
	s.Attempted = r.Tag
	if err := m.commit(s); err != nil {
		return err
	}
	if err := m.trigger(r.Tag); err != nil {
		s.Error = err.Error()
		if e := m.commit(s); e != nil {
			return e
		}
		return err
	}
	return nil
}
func (m *Manager) Handoff() *updater.Handoff {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plan == nil || !m.ready {
		return nil
	}
	return &updater.Handoff{Command: m.plan.Command, PlanPath: filepath.Join(m.directory, "update-service.json")}
}

func (m *Manager) DeliveryFailed(message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.DeliveryError == message {
		return nil
	}
	s := m.state
	s.DeliveryError = message
	return m.commit(s)
}

// Close cancels and joins background preparation before the service releases its
// resources. A completed handoff keeps the staged files for the offline updater.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.prepareCancel != nil {
		m.prepareCancel()
	}
	done := m.prepareDone
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}
func (m *Manager) prepare(ctx context.Context, cancel context.CancelFunc, done chan struct{}, p updater.ServicePlan, token string) {
	defer close(done)
	defer cancel()
	path := filepath.Join(m.directory, "update-service.json")
	cmd := exec.CommandContext(ctx, p.Command, "_prepare", path)
	// All managed updates use the control's token, including an explicit clear.
	// Keep it out of command arguments and the persisted handoff plan.
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token, "GITHUB_TOKEN=")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if token != "" {
			message = strings.ReplaceAll(message, token, "[redacted]")
		}
		err = fmt.Errorf("准备更新失败: %w: %s", err, message)
	}
	var prepared *updater.ServicePlan
	if err == nil {
		prepared, err = updater.ReadServicePlan(path)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.closed {
		if prepared != nil && prepared.Prepared != nil {
			os.RemoveAll(prepared.Prepared.Stage)
		}
		if err == nil {
			err = fmt.Errorf("更新准备已取消")
		}
	}
	if err == nil && prepared.Prepared != nil {
		m.plan = prepared
		m.ready = true
		m.Requested <- struct{}{}
		return
	}
	m.plan = nil
	result := updater.ServiceResult{State: "completed", Finished: time.Now().UTC()}
	s := m.state
	if err != nil {
		result.State = "failed"
		result.Error = err.Error()
		s.Error = err.Error()
	}
	if e := updater.WriteJSON(filepath.Join(m.directory, "update-result.json"), result); e != nil {
		s.Error = fmt.Sprintf("%s; 保存更新结果失败: %v", s.Error, e)
		log.Print(s.Error)
	}
	if e := m.commit(s); e != nil {
		m.state.Error = s.Error
		log.Printf("保存更新状态失败: %v", e)
	}
}
