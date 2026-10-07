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
)

func manager(t *testing.T, role string) *Manager {
	t.Helper()
	m, e := New(t.TempDir(), role, strings.Repeat("a", 32))
	if e != nil {
		t.Fatal(e)
	}
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
	if e := os.WriteFile(command, []byte("#!/bin/sh\n[ \"$1\" = --service-protocol ] && echo 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	configure(t, m, func(c *Config) { c.Command = command; c.Proxy = "http://127.0.0.1:7890"; c.Automatic = true })
	if e := m.Receive(release("new", "v0.3.3")); e != nil {
		t.Fatal(e)
	}
	if e := m.Automatic(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-m.Requested:
	default:
		t.Fatal("missing shutdown request")
	}
	if m.Handoff() == nil || m.plan.Proxy != "http://127.0.0.1:7890" || m.plan.Tag != "v0.3.3" {
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
