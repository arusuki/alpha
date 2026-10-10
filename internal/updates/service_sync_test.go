package updates

import (
	"os"
	"path/filepath"
	"testing"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/updater"
)

func TestAutomaticCompletesBinaryOnlyUpgradeOnce(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.3"
	defer func() { buildinfo.Version = old }()
	m := manager(t, "worker")
	m.executable = filepath.Join(m.directory, "project-alpha")
	command := filepath.Join(m.directory, "alpha-updater")
	if err := os.WriteFile(command, []byte(prepareHelperScript()), 0700); err != nil {
		t.Fatal(err)
	}
	configure(t, m, func(c *Config) { c.Command = command; c.Automatic = true })
	if err := m.Receive(release("current", "v0.3.3")); err != nil {
		t.Fatal(err)
	}
	// The old installer has already marked the binary release as attempted.
	s := m.state
	s.Attempted = "v0.3.3"
	if err := m.commit(s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"prepare-fail", "prepare-release"} {
		if err := os.WriteFile(filepath.Join(m.directory, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Automatic(); err != nil {
		t.Fatal(err)
	}
	<-m.prepareDone
	p, err := updater.ReadServicePlan(filepath.Join(m.directory, "update-service.json"))
	if err != nil || !p.ServicesOnly || p.Tag != buildinfo.Version {
		t.Fatal("missing same-version service preparation", p, err)
	}
	if m.Health()["update_state"] != "failed" {
		t.Fatal("failure missing")
	}
	restarted, err := New(m.directory, "worker", m.id)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Automatic(); err != nil || restarted.plan != nil {
		t.Fatal("failed sync repeated after restart", err)
	}
	if err := m.Trigger(buildinfo.Version); err != nil {
		t.Fatal("manual retry blocked", err)
	}
	<-m.prepareDone
}

func TestAutomaticSkipsCompletedServiceSync(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.3.3"
	defer func() { buildinfo.Version = old }()
	m := manager(t, "worker")
	configure(t, m, func(c *Config) { c.Automatic = true })
	if err := m.Receive(release("current", "v0.3.3")); err != nil {
		t.Fatal(err)
	}
	if err := updater.WriteJSON(filepath.Join(m.directory, "update-services.json"), map[string]string{"tag": "v0.3.3", "state": "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Automatic(); err != nil || m.plan != nil {
		t.Fatal("completed services updated again", err)
	}
}
