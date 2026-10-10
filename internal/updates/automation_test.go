package updates

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/updater"
)

func automaticManager(t *testing.T, role string) *Manager {
	t.Helper()
	m := manager(t, role)
	m.executable = filepath.Join(m.directory, "project-alpha")
	command := filepath.Join(m.directory, "alpha-updater")
	if err := os.WriteFile(command, []byte(prepareHelperScript()), 0700); err != nil {
		t.Fatal(err)
	}
	configure(t, m, func(c *Config) { c.Command = command })
	if err := os.WriteFile(filepath.Join(m.directory, "prepare-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	return m
}
func testVersion(t *testing.T) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = "v0.3.2"
	t.Cleanup(func() { buildinfo.Version = old })
}
func finishPreparation(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	done := m.prepareDone
	m.mu.Unlock()
	if done == nil {
		t.Fatal("preparation did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not finish")
	}
}
func retryNow(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	m.retry.Next = time.Now().Add(-time.Second)
	m.scheduleRetry(time.Millisecond)
	m.mu.Unlock()
}
func TestAutomaticReevaluatesSavedTargetForEveryRole(t *testing.T) {
	testVersion(t)
	for _, role := range []string{"control", "registry", "worker"} {
		t.Run(role, func(t *testing.T) {
			m := automaticManager(t, role)
			first := release("first", "v0.3.3")
			if err := m.Receive(first); err != nil {
				t.Fatal(err)
			}
			if err := m.Forwarded(first); err != nil {
				t.Fatal(err)
			}
			// Enabling automatic after acknowledgement must start the saved target.
			configure(t, m, func(c *Config) { c.Automatic = true })
			second := release("second", "v0.3.4")
			second.Published = second.Published.Add(time.Hour)
			if err := m.Receive(second); err != nil {
				t.Fatal(err)
			}
			if err := m.Automatic(); err != nil {
				t.Fatal(err)
			}
			finishPreparation(t, m)
			if m.plan.Tag != first.Tag {
				t.Fatal("in-flight plan changed")
			}
			m.Close()
			restarted, err := New(m.directory, role, m.id)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.executable = m.executable
			if err := restarted.Automatic(); err != nil {
				t.Fatal(err)
			}
			finishPreparation(t, restarted)
			if restarted.plan.Tag != second.Tag {
				t.Fatal("saved newer target was stranded")
			}
		})
	}
}
func TestPreparationRetriesPersistAndStopAtBudget(t *testing.T) {
	testVersion(t)
	m := automaticManager(t, "worker")
	configure(t, m, func(c *Config) { c.Automatic = true })
	if err := os.WriteFile(filepath.Join(m.directory, "prepare-fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	notice := release("first", "v0.3.3")
	if err := m.Receive(notice); err != nil {
		t.Fatal(err)
	}
	if err := m.Automatic(); err != nil {
		t.Fatal(err)
	}
	finishPreparation(t, m)
	if m.state.Attempted != "" || m.retry.Attempts != 1 || m.retryTimer == nil {
		t.Fatal("preparation consumed installation guard or lost retry")
	}
	m.Close()
	restarted, err := New(m.directory, "worker", m.id)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.executable = m.executable
	if err := restarted.Automatic(); err != nil {
		t.Fatal(err)
	}
	if restarted.plan != nil || restarted.retry.Attempts != 1 {
		t.Fatal("restart ignored persisted backoff")
	}
	for want := 2; want <= maxPreparationAttempts; want++ {
		restarted.mu.Lock()
		restarted.retry.Next = time.Now().Add(-time.Second)
		restarted.mu.Unlock()
		if err := restarted.Automatic(); err != nil {
			t.Fatal(err)
		}
		finishPreparation(t, restarted)
		if restarted.retry.Attempts != want {
			t.Fatal("retry budget not advanced")
		}
	}
	duplicate := notice
	duplicate.Delivery = "redelivery"
	duplicate.Published = duplicate.Published.Add(time.Hour)
	if err := restarted.Receive(duplicate); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Automatic(); err != nil {
		t.Fatal(err)
	}
	if restarted.plan != nil || restarted.retryTimer != nil || restarted.retry.Attempts != maxPreparationAttempts {
		t.Fatal("exhausted tag retried")
	}
}
func TestPreflightFailureTimerRecoversWithoutNewNotice(t *testing.T) {
	testVersion(t)
	m := automaticManager(t, "control")
	configure(t, m, func(c *Config) { c.Automatic = true; c.Command = filepath.Join(m.directory, "missing") })
	if err := m.Receive(release("first", "v0.3.3")); err != nil {
		t.Fatal(err)
	}
	if err := m.Automatic(); err == nil {
		t.Fatal("missing executable accepted")
	}
	if m.state.Attempted != "" {
		t.Fatal("preflight consumed installation guard")
	}
	configure(t, m, func(c *Config) { c.Command = filepath.Join(m.directory, "alpha-updater") })
	retryNow(t, m)
	select {
	case <-m.Requested:
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not retry")
	}
	finishPreparation(t, m)
	if m.Handoff() == nil || m.retry.Attempts != 2 || m.state.Attempted != "v0.3.3" {
		t.Fatal("retry did not prepare and guard handoff")
	}
}
func TestRecoveryMarkerBlocksAllAutomaticTargets(t *testing.T) {
	testVersion(t)
	m := automaticManager(t, "worker")
	configure(t, m, func(c *Config) { c.Automatic = true })
	if err := os.WriteFile(filepath.Join(m.directory, ".alpha-update-pending"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Receive(release("first", "v0.3.3")); err != nil {
		t.Fatal(err)
	}
	if err := m.Automatic(); err == nil {
		t.Fatal("recovery marker ignored")
	}
	if m.plan != nil || m.retryTimer != nil || m.retry.Attempts != 0 {
		t.Fatal("uncertain installation scheduled retry")
	}
}
func TestServicePreparationFailureDoesNotConsumeReceipt(t *testing.T) {
	testVersion(t)
	m := automaticManager(t, "worker")
	configure(t, m, func(c *Config) { c.Automatic = true })
	if err := os.WriteFile(filepath.Join(m.directory, "prepare-fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Receive(release("current", buildinfo.Version)); err != nil {
		t.Fatal(err)
	}
	if err := m.Automatic(); err != nil {
		t.Fatal(err)
	}
	finishPreparation(t, m)
	if attempted, err := updater.ServicesAttempted(m.directory, buildinfo.Version); err != nil || attempted {
		t.Fatal("failed preparation consumed service receipt", err)
	}
	if err := os.Remove(filepath.Join(m.directory, "prepare-fail")); err != nil {
		t.Fatal(err)
	}
	retryNow(t, m)
	select {
	case <-m.Requested:
	case <-time.After(5 * time.Second):
		t.Fatal("service preparation did not retry")
	}
	finishPreparation(t, m)
	if attempted, err := updater.ServicesAttempted(m.directory, buildinfo.Version); err != nil || !attempted {
		t.Fatal("prepared services lack installation guard", err)
	}
}
