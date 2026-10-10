package updates

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/credentials"
	"project-alpha/internal/updater"
)

func manager(t *testing.T, role string) *Manager {
	t.Helper()
	m, e := New(t.TempDir(), role, strings.Repeat("a", 32))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(m.Close)
	return m
}
func configure(t *testing.T, m *Manager, edit func(*Config)) {
	t.Helper()
	c := m.state.Config
	edit(&c)
	if e := m.Save(m.state.Revision, c); e != nil {
		t.Fatal(e)
	}
}
func release(id, tag string) Release {
	return Release{Delivery: id, Repo: "arusuki/alpha", Tag: tag, Published: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
}
func webhook(m *Manager, body, event, secret string) (int, error) {
	r := httptest.NewRequest("POST", WebhookPath, strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, _, err := m.Webhook(httptest.NewRecorder(), r)
	return status, err
}
func TestWebhookAuthenticationDurabilityAndDeduplication(t *testing.T) {
	m := manager(t, "registry")
	secret := strings.Repeat("s", 32)
	configure(t, m, func(c *Config) { c.WebhookSecret = secret })
	body := `{"action":"published","repository":{"full_name":"arusuki/alpha"},"release":{"tag_name":"v0.3.2","published_at":"2026-10-06T00:00:00Z"}}`
	if _, e := webhook(m, body, "release", "wrong"); e == nil {
		t.Fatal("accepted bad signature")
	}
	if _, e := webhook(m, strings.Replace(body, "arusuki/alpha", "other/repo", 1), "release", secret); e == nil {
		t.Fatal("accepted wrong repository")
	}
	if s, e := webhook(m, `{}`, "ping", secret); s != 200 || e != nil {
		t.Fatalf("ping %d %v", s, e)
	}
	if s, e := webhook(m, strings.Replace(body, "published", "edited", 1), "release", secret); s != 200 || e != nil || m.Pending() != nil {
		t.Fatal("processed edited release")
	}
	for range 2 {
		if s, e := webhook(m, body, "release", secret); s != 202 || e != nil {
			t.Fatalf("%d %v", s, e)
		}
	}
	if len(m.state.Outbox) != 1 {
		t.Fatal("duplicate was queued")
	}
	restarted, e := New(m.directory, "registry", m.id)
	if e != nil {
		t.Fatal(e)
	}
	notice := restarted.Pending()
	if notice == nil || notice.Tag != "v0.3.2" {
		t.Fatal("lost pending release")
	}
	if e = restarted.Forwarded(*notice); e != nil {
		t.Fatal(e)
	}
	restarted, e = New(m.directory, "registry", m.id)
	if e != nil || restarted.Pending() != nil {
		t.Fatal("lost acknowledgement")
	}
	if _, e = webhook(restarted, body, "release", secret); e != nil || restarted.Pending() != nil {
		t.Fatal("replayed acknowledged delivery")
	}
	stat, e := os.Stat(m.filename())
	if e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("settings permissions")
	}
	b, _ := json.Marshal(restarted.Settings())
	if strings.Contains(string(b), secret) {
		t.Fatal("secret leaked")
	}
}
func TestSettingsRevisionSecretsAndChannels(t *testing.T) {
	m := manager(t, "registry")
	configure(t, m, func(c *Config) { c.WebhookSecret = strings.Repeat("x", 32) })
	c := m.state.Config
	c.WebhookSecret = ""
	if e := m.Save(1, c); e == nil {
		t.Fatal("stale revision accepted")
	}
	if e := m.Save(m.state.Revision, c); e != nil || m.state.Config.WebhookSecret == "" {
		t.Fatal("blank erased secret")
	}
	c.ClearSecret = true
	if e := m.Save(m.state.Revision, c); e != nil || m.state.Config.WebhookSecret != "" {
		t.Fatal("clear failed")
	}
	stable := release("stable", "v0.3.2")
	pre := release("preview", "v0.3.3-rc.1")
	pre.Prerelease = true
	pre.Published = pre.Published.Add(time.Hour)
	for _, r := range []Release{stable, pre} {
		if e := m.Receive(r); e != nil {
			t.Fatal(e)
		}
	}
	if len(m.state.Outbox) != 2 || m.state.Release.Tag != stable.Tag {
		t.Fatal("registry channel filtered downstream notices or own stable candidate")
	}
	worker := manager(t, "worker")
	for _, r := range []Release{stable, pre} {
		if e := worker.Receive(r); e != nil {
			t.Fatal(e)
		}
	}
	if worker.state.Release.Tag != stable.Tag {
		t.Fatal("worker accepted preview")
	}
	old := release("old", "v0.3.1")
	old.Published = old.Published.Add(-time.Hour)
	worker.Receive(old)
	if worker.state.Release.Tag != stable.Tag {
		t.Fatal("reordered release replaced newer")
	}
}

func TestGitHubTokenSettings(t *testing.T) {
	for _, role := range []string{"control", "registry", "worker"} {
		t.Run(role, func(t *testing.T) {
			m := manager(t, role)
			const token = "test-configured-github-token"
			setToken := func(token string) {
				t.Helper()
				var err error
				if role == "control" {
					err = m.SaveGitHubToken(m.state.Revision, token, token == "")
				} else {
					err = m.ApplyGitHubToken(token)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			setToken(token)
			raw, err := os.ReadFile(m.filename())
			if err != nil || strings.Contains(string(raw), token) || strings.Contains(string(raw), `"github_token":`) {
				t.Fatal("plaintext token persisted", err)
			}
			var persisted state
			if err := json.Unmarshal(raw, &persisted); err != nil || !strings.HasPrefix(persisted.GitHubTokenCiphertext, credentials.Prefix) {
				t.Fatal("missing encrypted token", err)
			}
			keyInfo, err := os.Stat(filepath.Join(m.directory, githubTokenKeyFile))
			if err != nil || keyInfo.Mode().Perm() != 0600 {
				t.Fatal("token encryption key permissions", err)
			}
			for _, value := range []any{m.Settings(), m.Health()} {
				raw, err := json.Marshal(value)
				if err != nil || strings.Contains(string(raw), token) {
					t.Fatal("token leaked in API response", err)
				}
			}
			visible := m.Settings().(map[string]any)["config"].(Config)
			if !visible.HasGitHubToken || visible.GitHubToken != "" {
				t.Fatal("missing token status or exposed token")
			}
			// Saving the read-only response with a blank token preserves it.
			if err := m.Save(m.state.Revision, visible); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(m.directory, role, m.id)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if restarted.state.Config.GitHubToken != token || restarted.state.Config.HasGitHubToken || restarted.state.Config.ClearGitHubToken {
				t.Fatal("token was not persisted correctly")
			}
			info, err := os.Stat(m.filename())
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("token settings permissions", err)
			}
			setToken("replacement-token")
			if m.state.Config.GitHubToken != "replacement-token" {
				t.Fatal("token replacement failed")
			}
			setToken("")
			if m.state.Config.GitHubToken != "" || m.Settings().(map[string]any)["config"].(Config).HasGitHubToken {
				t.Fatal("token clear failed")
			}
			raw, err = os.ReadFile(m.filename())
			if err != nil || strings.Contains(string(raw), "github_token_ciphertext") || strings.Contains(string(raw), "replacement-token") {
				t.Fatal("cleared token retained on disk", err)
			}
			for _, invalid := range []string{"has space", "has\nnewline", "has\x00nul", "非ASCII", strings.Repeat("x", 4097)} {
				var err error
				if role == "control" {
					err = m.SaveGitHubToken(m.state.Revision, invalid, false)
				} else {
					err = m.ApplyGitHubToken(invalid)
				}
				if err == nil || m.state.Config.GitHubToken != "" {
					t.Fatal("accepted invalid token")
				}
			}
		})
	}
}

func TestGitHubTokenEncryptionRejectsMissingOrDamagedKey(t *testing.T) {
	for _, role := range []string{"control", "registry", "worker"} {
		for _, damage := range []string{"missing", "corrupt", "permissions"} {
			t.Run(role+"/"+damage, func(t *testing.T) {
				m := manager(t, role)
				setToken := func(token string) error {
					if role == "control" {
						return m.SaveGitHubToken(m.state.Revision, token, token == "")
					}
					return m.ApplyGitHubToken(token)
				}
				const token = "secret-encryption-fixture"
				if err := setToken(token); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(m.filename())
				if err != nil {
					t.Fatal(err)
				}
				keyPath := filepath.Join(m.directory, githubTokenKeyFile)
				switch damage {
				case "missing":
					err = os.Remove(keyPath)
				case "corrupt":
					err = os.WriteFile(keyPath, []byte("invalid"), 0600)
				case "permissions":
					err = os.Chmod(keyPath, 0644)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := New(m.directory, role, m.id); err == nil {
					t.Fatal("reopened with unavailable key")
				}
				for _, next := range []string{token, "replacement", ""} {
					if err := setToken(next); err == nil {
						t.Fatal("changed credentials without original key")
					}
				}
				after, err := os.ReadFile(m.filename())
				if err != nil || string(after) != string(before) || m.state.Config.GitHubToken != token {
					t.Fatal("failed encryption operation changed settings", err)
				}
				if damage == "missing" {
					if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
						t.Fatal("recreated missing key", err)
					}
				}
			})
		}
	}
}

func TestGitHubTokenEncryptionRejectsPlaintextTamperingAndWrongIdentity(t *testing.T) {
	for _, damage := range []string{"plaintext", "ciphertext", "identity"} {
		t.Run(damage, func(t *testing.T) {
			m := manager(t, "control")
			if err := m.SaveGitHubToken(1, "confidential-fixture", false); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(m.filename())
			if err != nil {
				t.Fatal(err)
			}
			var persisted state
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			id := m.id
			switch damage {
			case "plaintext":
				persisted.Config.GitHubToken = "confidential-fixture"
				persisted.GitHubTokenCiphertext = ""
			case "ciphertext":
				persisted.GitHubTokenCiphertext = credentials.Prefix + strings.Repeat("A", 64)
			case "identity":
				id = strings.Repeat("b", 32)
			}
			if err := updater.WriteJSON(m.filename(), persisted); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(m.filename())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(m.directory, "control", id); err == nil || strings.Contains(err.Error(), "confidential-fixture") {
				t.Fatal("accepted invalid credential or exposed it in error")
			}
			after, err := os.ReadFile(m.filename())
			if err != nil || string(before) != string(after) {
				t.Fatal("overwrote invalid credential", err)
			}
		})
	}
}

func TestGitHubTokenPassedToUpdater(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.2"
	defer func() { buildinfo.Version = old }()
	for _, tc := range []struct {
		name, role, configured, gh, github, expected string
		automatic, fail                              bool
	}{
		{name: "manual", configured: "configured-token", gh: "environment-token", github: "fallback-token", expected: "configured-token"},
		{name: "automatic", configured: "configured-token", expected: "configured-token", automatic: true},
		{name: "gh-environment", gh: "environment-token", github: "fallback-token", expected: "environment-token"},
		{name: "github-environment", github: "fallback-token", expected: "fallback-token"},
		{name: "redacted-error", configured: "configured-token", expected: "configured-token", fail: true},
		{name: "worker-delivery", role: "worker", configured: "shared-token", gh: "node-local-token", github: "node-fallback", expected: "shared-token", automatic: true},
		{name: "worker-cleared-token", role: "worker", gh: "node-local-token", github: "node-fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			t.Setenv("ALPHA_TEST_EXPECT_GITHUB_TOKEN", tc.expected)
			role := tc.role
			if role == "" {
				role = "control"
			}
			m := manager(t, role)
			m.executable = filepath.Join(m.directory, "project-alpha")
			command := filepath.Join(m.directory, "alpha-updater")
			if err := os.WriteFile(command, []byte(prepareHelperScript()), 0700); err != nil {
				t.Fatal(err)
			}
			configure(t, m, func(c *Config) { c.Command = command; c.Automatic = tc.automatic })
			if role == "control" {
				if err := m.SaveGitHubToken(m.state.Revision, tc.configured, false); err != nil {
					t.Fatal(err)
				}
			} else if err := m.ApplyGitHubToken(tc.configured); err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				if err := os.WriteFile(filepath.Join(m.directory, "prepare-fail"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(m.directory, "prepare-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if tc.automatic {
				if err := m.Receive(release("new", "v0.3.3")); err != nil {
					t.Fatal(err)
				}
				if err := m.Forwarded(release("new", "v0.3.3")); err != nil {
					t.Fatal(err)
				}
				if err := m.Automatic(); err != nil {
					t.Fatal(err)
				}
			} else if err := m.Trigger(""); err != nil {
				t.Fatal(err)
			}
			select {
			case <-m.prepareDone:
			case <-time.After(5 * time.Second):
				t.Fatal("preparation timed out")
			}
			if tc.fail {
				if m.Handoff() != nil || !strings.Contains(m.state.Error, "[redacted]") {
					t.Fatal("missing redacted failure")
				}
				raw, err := os.ReadFile(filepath.Join(m.directory, "update-result.json"))
				if err != nil || strings.Contains(string(raw), tc.expected) {
					t.Fatal("token leaked in update result", err)
				}
				raw, _ = json.Marshal(m.Health())
				if strings.Contains(string(raw), tc.expected) {
					t.Fatal("token leaked in health response")
				}
			} else if m.Handoff() == nil {
				t.Fatal("authenticated preparation failed")
			}
			raw, err := os.ReadFile(filepath.Join(m.directory, "update-service.json"))
			if err != nil || tc.expected != "" && strings.Contains(string(raw), tc.expected) {
				t.Fatal("token leaked in handoff", err)
			}
		})
	}
}

func TestNodesOnlyAcceptSharedGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "local-token")
	t.Setenv("GITHUB_TOKEN", "local-fallback")
	for _, role := range []string{"registry", "worker"} {
		m := manager(t, role)
		if m.GitHubToken() != "" {
			t.Fatal("node used its independent environment token")
		}
		c := m.state.Config
		c.GitHubToken = "independent-token"
		if err := m.Save(m.state.Revision, c); err == nil {
			t.Fatal("node accepted independently configured token")
		}
		if err := m.SaveGitHubToken(m.state.Revision, "independent-token", false); err == nil {
			t.Fatal("node accepted central configuration operation")
		}
		for range 2 {
			if err := m.ApplyGitHubToken("shared-token"); err != nil {
				t.Fatal(err)
			}
		}
		if m.GitHubToken() != "shared-token" || m.state.Revision != 1 {
			t.Fatal("delivery lost token or invalidated local settings revision")
		}
		if err := m.ApplyGitHubToken(""); err != nil || m.GitHubToken() != "" {
			t.Fatal("cleared token fell back to a node-local token", err)
		}
	}
}
func TestUpdateHandoffAndDuplicatePrevention(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.2"
	defer func() { buildinfo.Version = old }()
	m := manager(t, "worker")
	m.executable = filepath.Join(m.directory, "project-alpha")
	command := filepath.Join(m.directory, "alpha-updater")
	if e := os.WriteFile(command, []byte(prepareHelperScript()), 0700); e != nil {
		t.Fatal(e)
	}
	configure(t, m, func(c *Config) { c.Command = command; c.Proxy = "http://127.0.0.1:7890"; c.Automatic = true })
	if e := m.Receive(release("new", "v0.3.3")); e != nil {
		t.Fatal(e)
	}
	if e := m.Automatic(); e != nil {
		t.Fatal(e)
	}
	waitFile(t, filepath.Join(m.directory, "prepare-started"))
	if m.Health()["update_state"] != "downloading" || m.Handoff() != nil {
		t.Fatal("service handed off before preparation")
	}
	select {
	case <-m.Requested:
		t.Fatal("stopped during download")
	default:
	}
	if err := os.WriteFile(filepath.Join(m.directory, "prepare-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Requested:
	case <-time.After(5 * time.Second):
		t.Fatal("missing shutdown request")
	}
	if m.Handoff() == nil || m.plan.Proxy != "http://127.0.0.1:7890" || m.plan.Tag != "v0.3.3" || !m.plan.Published {
		t.Fatal("lost update parameters")
	}
	if e := m.Trigger(""); e == nil {
		t.Fatal("duplicate update accepted")
	}
	restarted, e := New(m.directory, "worker", m.id)
	if e != nil {
		t.Fatal(e)
	}
	if e = restarted.Automatic(); e != nil || restarted.plan != nil {
		t.Fatal("restart repeated automatic update")
	}
}
func TestInvalidFormatPreservesFile(t *testing.T) {
	m := manager(t, "worker")
	raw := []byte(`{"format":999}`)
	os.WriteFile(m.filename(), raw, 0600)
	if _, e := New(m.directory, "worker", m.id); e == nil {
		t.Fatal("accepted unknown format")
	}
	got, _ := os.ReadFile(m.filename())
	if string(got) != string(raw) {
		t.Fatal("overwrote invalid settings")
	}
}

func prepareHelperScript() string {
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	return "#!/bin/sh\nif [ \"$1\" = --service-protocol ]; then echo 3; exit 0; fi\nALPHA_TEST_PREPARE_PLAN=\"$2\" exec " + quote(os.Args[0]) + " -test.run=^TestUpdatePrepareProcess$\n"
}
func waitFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
func TestUpdatePrepareProcess(t *testing.T) {
	path := os.Getenv("ALPHA_TEST_PREPARE_PLAN")
	if path == "" {
		return
	}
	p, err := updater.ReadServicePlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if expected, check := os.LookupEnv("ALPHA_TEST_EXPECT_GITHUB_TOKEN"); check {
		actual := os.Getenv("GH_TOKEN")
		if actual == "" {
			actual = os.Getenv("GITHUB_TOKEN")
		}
		if actual != expected || expected != "" && strings.Contains(strings.Join(os.Args, " "), expected) {
			t.Fatal("updater token environment or arguments are incorrect")
		}
	}
	if err := os.WriteFile(filepath.Join(p.Directory, "prepare-started"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(p.Directory, "prepare-release"))
	if _, err := os.Stat(filepath.Join(p.Directory, "prepare-fail")); err == nil {
		if token := os.Getenv("ALPHA_TEST_EXPECT_GITHUB_TOKEN"); token != "" {
			os.Stderr.WriteString("updater error containing " + token)
		}
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(p.Directory, "prepare-noop")); err == nil {
		return
	}
	stage, err := os.MkdirTemp(filepath.Dir(p.Executable), ".alpha-stage-")
	if err != nil {
		t.Fatal(err)
	}
	p.Prepared = &updater.PreparedUpdate{Stage: stage, Tag: p.Tag, Installed: "v0.3.2"}
	if p.Prepared.Tag == "" {
		p.Prepared.Tag = "v0.3.3"
	}
	if err := updater.WriteJSON(path, p); err != nil {
		t.Fatal(err)
	}
}
func TestPreparationFailureAndNoopKeepServiceRunning(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.2"
	defer func() { buildinfo.Version = old }()
	for _, outcome := range []string{"fail", "noop"} {
		t.Run(outcome, func(t *testing.T) {
			m := manager(t, "worker")
			m.executable = filepath.Join(m.directory, "project-alpha")
			command := filepath.Join(m.directory, "alpha-updater")
			if err := os.WriteFile(command, []byte(prepareHelperScript()), 0700); err != nil {
				t.Fatal(err)
			}
			configure(t, m, func(c *Config) { c.Command = command; c.Automatic = true })
			for _, name := range []string{"prepare-" + outcome, "prepare-release"} {
				if err := os.WriteFile(filepath.Join(m.directory, name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Receive(release("new", "v0.3.3")); err != nil {
				t.Fatal(err)
			}
			if err := m.Automatic(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-m.prepareDone:
			case <-time.After(5 * time.Second):
				t.Fatal("preparation did not finish")
			}
			select {
			case <-m.Requested:
				t.Fatal("service stopped")
			default:
			}
			want := "completed"
			if outcome == "fail" {
				want = "failed"
			}
			if m.Handoff() != nil || m.Health()["update_state"] != want {
				t.Fatal(m.Health())
			}
			if err := m.Automatic(); err != nil || m.plan != nil {
				t.Fatal("repeated automatic attempt", err)
			}
			restarted, err := New(m.directory, "worker", m.id)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if restarted.Health()["update_state"] != want {
				t.Fatal("result not persisted")
			}
			if err := m.Trigger(""); err != nil {
				t.Fatal("manual retry rejected", err)
			}
			<-m.prepareDone
		})
	}
}

func TestPreparationCanceledOnClose(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.2"
	defer func() { buildinfo.Version = old }()
	m := manager(t, "worker")
	m.executable = filepath.Join(m.directory, "project-alpha")
	command := filepath.Join(m.directory, "alpha-updater")
	if err := os.WriteFile(command, []byte(prepareHelperScript()), 0700); err != nil {
		t.Fatal(err)
	}
	configure(t, m, func(c *Config) { c.Command = command })
	if err := m.Trigger(""); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(m.directory, "prepare-started"))
	m.Close()
	select {
	case <-m.Requested:
		t.Fatal("canceled preparation requested shutdown")
	default:
	}
	if m.Handoff() != nil || m.Health()["update_state"] != "failed" {
		t.Fatal(m.Health())
	}
	if err := m.Trigger(""); err == nil {
		t.Fatal("closed manager accepted update")
	}
}
