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
	return "#!/bin/sh\nif [ \"$1\" = --service-protocol ]; then echo 2; exit 0; fi\nALPHA_TEST_PREPARE_PLAN=\"$2\" exec " + quote(os.Args[0]) + " -test.run=^TestUpdatePrepareProcess$\n"
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
	if err := os.WriteFile(filepath.Join(p.Directory, "prepare-started"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(p.Directory, "prepare-release"))
	if _, err := os.Stat(filepath.Join(p.Directory, "prepare-fail")); err == nil {
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
