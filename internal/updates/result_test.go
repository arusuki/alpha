package updates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/updater"
)

func TestFailureReportAndSuccessfulManualRetry(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.8.0-rc1"
	t.Cleanup(func() { buildinfo.Version = old })
	for _, role := range []string{"control", "worker", "registry"} {
		t.Run(role, func(t *testing.T) {
			m := manager(t, role)
			m.executable = filepath.Join(m.directory, "bin", "project-alpha")
			configure(t, m, func(c *Config) {
				c.Automatic = true
				c.Command = filepath.Join(m.directory, "missing-updater")
				c.Prerelease = true
			})
			path := filepath.Join(m.directory, "update-result.json")
			// A new preflight failure must supersede a previous successful update.
			if err := updater.WriteJSON(path, m.resultPlan("").Result(nil)); err != nil {
				t.Fatal(err)
			}
			notice := release("new", "v0.8.0-rc2")
			notice.Prerelease = true
			if err := m.Receive(notice); err != nil {
				t.Fatal(err)
			}
			if err := m.Forwarded(notice); err != nil {
				t.Fatal(err)
			}
			if err := m.Automatic(); err == nil {
				t.Fatal("missing updater accepted")
			}
			health := m.Health()
			if health["update_state"] != "failed" || health["error"] == "" {
				t.Fatalf("failure hidden by old success: %v", health)
			}
			recovery := health["recovery"].(*updater.RecoveryInstructions)
			log, err := os.ReadFile(recovery.LogPath)
			if err != nil || !strings.Contains(string(log), health["error"].(string)) {
				t.Fatal("preflight failure absent from log", err)
			}
			for _, arg := range []string{m.directory, "--role", role, "--tag", notice.Tag, "--prerelease"} {
				if !strings.Contains(recovery.Command, arg) {
					t.Fatalf("retry missing %q: %s", arg, recovery.Command)
				}
			}
			if err := m.DeliveryFailed("downstream unavailable"); err != nil {
				t.Fatal(err)
			}
			// An offline CLI invocation publishes its own result without this manager.
			if err := updater.WriteJSON(path, m.resultPlan(notice.Tag).Result(nil)); err != nil {
				t.Fatal(err)
			}
			health = m.Health()
			if health["update_state"] != "completed" || health["error"] != "" || health["recovery"].(*updater.RecoveryInstructions) != nil || m.state.Error != "" {
				t.Fatalf("manual success retained old failure: %v", health)
			}
			if health["delivery_error"] != "downstream unavailable" {
				t.Fatal("update success erased an unrelated delivery failure")
			}
			raw, err := os.ReadFile(m.filename())
			var saved state
			if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Error != "" {
				t.Fatal("failed marker still persisted", err)
			}
			restarted, err := New(m.directory, role, m.id)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if restarted.Health()["update_state"] != "completed" {
				t.Fatal("failure returned after restart")
			}
			if err := m.Trigger(notice.Tag); err == nil || m.Health()["update_state"] != "failed" {
				t.Fatal("manual trigger preflight failure was not persisted")
			}
		})
	}
}

func TestInvalidResultAndPendingRecovery(t *testing.T) {
	m := manager(t, "worker")
	m.executable = filepath.Join(m.directory, "project-alpha")
	path := filepath.Join(m.directory, "update-result.json")
	if err := os.WriteFile(path, []byte(`{"state":"completed","error":"stale"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if h := m.Health(); h["update_state"] != "failed" || !strings.Contains(h["error"].(string), "无效") {
		t.Fatal("invalid success accepted", h)
	}
	if err := m.Trigger("v0.8.0-rc2"); err == nil {
		t.Fatal("missing updater accepted")
	}
	marker := filepath.Join(m.directory, ".alpha-update-pending")
	if err := os.WriteFile(marker, []byte("recovery required"), 0600); err != nil {
		t.Fatal(err)
	}
	if !m.Health()["recovery"].(*updater.RecoveryInstructions).Required {
		t.Fatal("new recovery marker not reported")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if m.Health()["recovery"].(*updater.RecoveryInstructions).Required {
		t.Fatal("resolved recovery marker still reported")
	}
}
