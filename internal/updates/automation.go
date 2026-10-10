package updates

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/updater"
)

// This journal covers preparation only. Attempted and the service receipt guard
// the separate, irreversible handoff to the installer.
type preparationRetry struct {
	Format       int       `json:"format"`
	Repo         string    `json:"repo"`
	Tag          string    `json:"tag"`
	ServicesOnly bool      `json:"services_only"`
	Attempts     int       `json:"attempts"`
	Next         time.Time `json:"next"`
}

const maxPreparationAttempts = 3

func (m *Manager) loadRetry() error {
	raw, err := os.ReadFile(filepath.Join(m.directory, "update-retry.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var retry preparationRetry
	if json.Unmarshal(raw, &retry) != nil || retry.Format != 1 || !repoPattern.MatchString(retry.Repo) || updater.ValidateTag(retry.Tag) != nil || retry.Attempts < 1 || retry.Attempts > maxPreparationAttempts || retry.Next.IsZero() {
		return fmt.Errorf("invalid update retry journal; use a new data directory")
	}
	m.retry = retry
	return nil
}

func (m *Manager) stopRetry() {
	if m.retryTimer != nil {
		m.retryTimer.Stop()
		m.retryTimer = nil
	}
}

func (m *Manager) scheduleRetry(delay time.Duration) {
	m.stopRetry()
	m.retryTimer = time.AfterFunc(delay, func() {
		if err := m.Automatic(); err != nil {
			log.Printf("自动更新重试失败: %v", err)
		}
	})
}

// Call under mu after saving settings. Saving is successful even when the
// independent update preflight fails; that failure has its own persisted report.
func (m *Manager) reevaluate() {
	if err := m.automatic(); err != nil {
		log.Printf("自动更新检查失败: %v", err)
	}
}

func (m *Manager) automatic() error {
	m.stopRetry()
	r := m.state.Release
	c := m.state.Config
	if m.closed || !c.Automatic || r == nil || m.plan != nil || r.Repo != c.Repo || r.Prerelease && !c.Prerelease {
		return nil
	}
	services := m.role == "worker" && r.Tag == buildinfo.Version
	if services {
		attempted, err := updater.ServicesAttempted(m.directory, r.Tag)
		if err != nil {
			m.recordResult(m.resultPlan(r.Tag), err)
			return err
		}
		if attempted {
			return nil
		}
	} else if m.state.Attempted == r.Tag || !updater.Newer(r.Tag, buildinfo.Version) {
		return nil
	}
	// Never retry an installation whose recovery state is unknown, even for a
	// newer notification. A stat error is also unsafe to interpret as absence.
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.executable), ".alpha-update-pending")); !os.IsNotExist(err) {
		err = fmt.Errorf("上次更新需要人工恢复，请检查 .alpha-update-pending")
		m.recordResult(m.resultPlan(r.Tag), err)
		return err
	}
	retry := m.retry
	if retry.Repo != r.Repo || retry.Tag != r.Tag || retry.ServicesOnly != services {
		retry = preparationRetry{Format: 1, Repo: r.Repo, Tag: r.Tag, ServicesOnly: services}
	}
	if retry.Attempts >= maxPreparationAttempts {
		return nil
	}
	if delay := time.Until(retry.Next); delay > 0 {
		m.scheduleRetry(delay)
		return nil
	}
	retry.Attempts++
	delay := time.Minute
	if retry.Attempts > 1 {
		delay = 5 * time.Minute
	}
	retry.Next = time.Now().Add(delay)
	// Persist the budget before spawning so crashes/restarts cannot reset it.
	if err := updater.WriteJSON(filepath.Join(m.directory, "update-retry.json"), retry); err != nil {
		return err
	}
	m.retry = retry
	if err := m.trigger(r.Tag); err != nil {
		m.recordResult(m.resultPlan(r.Tag), err)
		if m.delayFailedPreparation(r.Tag) && retry.Attempts < maxPreparationAttempts {
			m.scheduleRetry(time.Until(m.retry.Next))
		}
		return err
	}
	return nil
}

// Measure backoff from the failure, including downloads that ran longer than
// the delay. The initial deadline also covers a process exit during preparation.
func (m *Manager) delayFailedPreparation(tag string) bool {
	if m.retry.Tag != tag || m.retry.Repo != m.state.Config.Repo || m.retry.Attempts == 0 || m.retry.Attempts >= maxPreparationAttempts {
		return true
	}
	retry := m.retry
	delay := time.Minute
	if retry.Attempts > 1 {
		delay = 5 * time.Minute
	}
	retry.Next = time.Now().Add(delay)
	if err := updater.WriteJSON(filepath.Join(m.directory, "update-retry.json"), retry); err != nil {
		log.Printf("保存更新重试时间失败: %v", err)
		return false
	}
	m.retry = retry
	return true
}
