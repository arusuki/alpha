package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestCalendarSchedule(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	c := defaultConfig()
	c.ScheduleMode, c.ScheduleTimezone = "calendar", "Asia/Shanghai"
	c.ScheduleTimes, c.ScheduleWeekdays = []string{"02:30", "14:30"}, []int{4}
	check := func(instant string, want bool) {
		t.Helper()
		now, err := time.Parse(time.RFC3339, instant)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.scheduleDue(c, now)
		if err != nil || got != want {
			t.Fatalf("%s: due=%v want=%v: %v", instant, got, want, err)
		}
	}
	check("2026-10-07T18:30:00Z", true)
	check("2026-10-08T06:30:59Z", true)
	check("2026-10-08T06:31:00Z", false)
	check("2026-10-09T06:30:00Z", false)
	last, _ := time.Parse(time.RFC3339, "2026-10-08T06:30:01Z")
	if _, err = db.SQL.Exec("UPDATE settings SET schedule_last_run=?", last.Unix()); err != nil {
		t.Fatal(err)
	}
	check("2026-10-08T06:30:59Z", false)
	check("2026-10-15T06:30:00Z", true)
	// DST repeats are separate real minutes; nonexistent local times never match.
	c.ScheduleTimezone, c.ScheduleTimes, c.ScheduleWeekdays = "America/New_York", []string{"01:30"}, []int{0}
	check("2026-11-01T05:30:00Z", true)
	check("2026-11-01T06:30:00Z", true)
	c.ScheduleTimes = []string{"02:30"}
	check("2027-03-14T07:30:00Z", false)
	c.ScheduleMode = "off"
	check("2026-10-15T06:30:00Z", false)
}

func TestScheduleValidation(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"mode":                func(c *Config) { c.ScheduleMode = "invalid" },
		"interval":            func(c *Config) { c.ScheduleMode = "interval"; c.IntervalMinutes = 0 },
		"timezone":            func(c *Config) { c.ScheduleTimezone = "Mars/Base" },
		"local timezone":      func(c *Config) { c.ScheduleTimezone = "Local" },
		"missing time":        func(c *Config) { c.ScheduleMode = "calendar" },
		"invalid time":        func(c *Config) { c.ScheduleTimes = []string{"24:00"} },
		"short time":          func(c *Config) { c.ScheduleTimes = []string{"2:00"} },
		"duplicate time":      func(c *Config) { c.ScheduleTimes = []string{"02:00", "02:00"} },
		"weekday":             func(c *Config) { c.ScheduleWeekdays = []int{7} },
		"duplicate day":       func(c *Config) { c.ScheduleWeekdays = []int{1, 1} },
		"null days":           func(c *Config) { c.ScheduleWeekdays = nil },
		"null times":          func(c *Config) { c.ScheduleTimes = nil },
		"negative retention":  func(c *Config) { c.RetainRecords = -1 },
		"excessive retention": func(c *Config) { c.RetainRecords = 10001 },
	} {
		t.Run(name, func(t *testing.T) {
			c := defaultConfig()
			edit(&c)
			if _, err := parseConfig([]byte(httpapi.JSONText(c))); err == nil {
				t.Fatal("invalid schedule accepted")
			}
		})
	}
}

func TestScheduledLaunchFailureCursorSurvivesDeletionAndRestart(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	c := defaultConfig()
	c.ScheduleMode = "interval"
	c.IntervalMinutes = 5
	if _, err = db.saveConfig(c, 1, platform.User{ID: "admin", Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(db)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.command = func(string, string) *exec.Cmd { return exec.Command("/nonexistent-alpha-scheduler") }
	m.mu.Unlock()
	if err = m.tick(); err == nil {
		t.Fatal("expected launch failure")
	}
	jobs, err := db.jobs(platform.Now() + 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", jobs, err)
	}
	if _, err = m.Delete(jobs[0]["id"].(string), "admin"); err != nil {
		t.Fatal(err)
	}
	m.Close()
	m, err = NewManager(db)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err = m.tick(); err != nil {
		t.Fatal(err)
	}
	jobs, err = db.jobs(platform.Now() + 1)
	if err != nil || len(jobs) != 0 {
		t.Fatal("deleted failed job was retried before interval", jobs, err)
	}
	if due, err := db.scheduleDue(c, time.Now().Add(5*time.Minute)); err != nil || !due {
		t.Fatal("interval never resumed", err)
	}
}

func TestRetentionPrunesWholeRecordsAndDefersActiveChildren(t *testing.T) {
	p := newTestPlatform(t)
	old := deleteFixture(t, p, "completed", "manual", "")
	child := deleteFixture(t, p, "running", "incremental", old)
	newest := deleteFixture(t, p, "failed", "scheduled", "")
	if _, err := p.db.SQL.Exec("INSERT INTO snapshot_records VALUES(?,1,'/','{}'); INSERT INTO snapshot_nodes VALUES(?,'/','',0,'{}',NULL)", old, old); err != nil {
		t.Fatal(err)
	}
	func() {
		p.m.mu.Lock()
		defer p.m.mu.Unlock()
		if err := p.m.pruneLocked(1); err == nil {
			t.Fatal("pruned running child")
		}
		if _, err := p.db.SQL.Exec("UPDATE jobs SET status='completed' WHERE id=?", child); err != nil {
			t.Fatal(err)
		}
		if err := p.m.pruneLocked(0); err != nil {
			t.Fatal(err)
		}
		if _, err := p.db.job(old); err != nil {
			t.Fatal("zero retention deleted records")
		}
		if err := p.m.pruneLocked(1); err != nil {
			t.Fatal(err)
		}

	}()
	for _, id := range []string{old, child} {
		if _, err := p.db.job(id); err == nil {
			t.Fatal("retained expired job")
		}
		if _, err := os.Stat(filepath.Join(p.db.Directory, "results", id)); !os.IsNotExist(err) {
			t.Fatal("result files remain", err)
		}
	}
	if _, err := p.db.job(newest); err != nil {
		t.Fatal("removed latest job", err)
	}
	var n int
	if err := p.db.SQL.QueryRow("SELECT count(*) FROM snapshot_nodes").Scan(&n); err != nil || n != 0 {
		t.Fatal("snapshot rows remain", err)
	}
	if _, err := os.Stat(filepath.Join(p.storage, "model.bin")); err != nil {
		t.Fatal("source changed", err)
	}
}

func TestScheduledEnqueueRollsBackCursor(t *testing.T) {
	p := newTestPlatform(t)
	if _, err := p.db.SQL.Exec("CREATE TRIGGER reject_schedule BEFORE INSERT ON audit WHEN NEW.action='scan.start' BEGIN SELECT RAISE(ABORT,'reject'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.Start("scheduler", "scheduled"); err == nil {
		t.Fatal("expected transaction failure")
	}
	var last float64
	if err := p.db.SQL.QueryRow("SELECT schedule_last_run FROM settings").Scan(&last); err != nil || last != 0 {
		t.Fatal("cursor advanced after rollback", last, err)
	}
	jobs, err := p.db.jobs(platform.Now() + 1)
	if err != nil || len(jobs) != 0 {
		t.Fatal("job persisted after rollback", err)
	}
}
