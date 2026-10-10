package updates

import (
	"path/filepath"
	"testing"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/updater"
)

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
