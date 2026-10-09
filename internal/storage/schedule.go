package storage

import (
	"fmt"
	"slices"
	"time"
	_ "time/tzdata"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (c Config) validateSchedule() error {
	if c.ScheduleMode != "off" && c.ScheduleMode != "interval" && c.ScheduleMode != "calendar" {
		return httpapi.NewError(400, "自动扫描模式必须是 off、interval 或 calendar")
	}
	if c.ScheduleMode == "interval" && c.IntervalMinutes < 5 {
		return httpapi.NewError(400, "定时扫描间隔至少为 5 分钟")
	}
	if c.RetainRecords < 0 || c.RetainRecords > 10000 {
		return httpapi.NewError(400, "扫描记录保留数量必须在 0–10000 之间，0 表示不自动清理")
	}
	if c.ScheduleTimezone == "" || c.ScheduleTimezone == "Local" || len(c.ScheduleTimezone) > 100 {
		return httpapi.NewError(400, "请指定有效的 IANA 时区，例如 Asia/Shanghai 或 UTC")
	}
	if _, err := time.LoadLocation(c.ScheduleTimezone); err != nil {
		return httpapi.NewError(400, "扫描计划时区无效")
	}
	if c.ScheduleTimes == nil || len(c.ScheduleTimes) > 24 || c.ScheduleWeekdays == nil || len(c.ScheduleWeekdays) > 7 {
		return httpapi.NewError(400, "时间计划最多包含 24 个时刻和 7 个星期选项")
	}
	seen := map[string]bool{}
	for _, clock := range c.ScheduleTimes {
		if _, err := time.Parse("15:04", clock); err != nil || len(clock) != 5 || seen[clock] {
			return httpapi.NewError(400, "执行时刻必须为不重复的 HH:MM（24 小时制）")
		}
		seen[clock] = true
	}
	days := map[int]bool{}
	for _, day := range c.ScheduleWeekdays {
		if day < 0 || day > 6 || days[day] {
			return httpapi.NewError(400, "星期必须是不重复的 0–6（0 为周日）")
		}
		days[day] = true
	}
	if c.ScheduleMode == "calendar" && (len(c.ScheduleTimes) == 0 || len(c.ScheduleWeekdays) == 0) {
		return httpapi.NewError(400, "时间计划必须选择至少一个星期和执行时刻")
	}
	return nil
}

// Only the current minute is eligible: downtime and long scans do not create
// a backlog. The persisted cursor prevents repeat launches after a restart,
// failed process launch, or retention/manual deletion of the previous job.
func (d *Store) scheduleDue(c Config, now time.Time) (bool, error) {
	if c.ScheduleMode == "off" {
		return false, nil
	}
	var last float64
	if err := d.SQL.QueryRow("SELECT schedule_last_run FROM settings WHERE id=1").Scan(&last); err != nil {
		return false, err
	}
	if c.ScheduleMode == "calendar" {
		loc, err := time.LoadLocation(c.ScheduleTimezone)
		if err != nil {
			return false, err
		}
		local := now.In(loc)
		return slices.Contains(c.ScheduleWeekdays, int(local.Weekday())) && slices.Contains(c.ScheduleTimes, local.Format("15:04")) && last < float64(now.Truncate(time.Minute).Unix()), nil
	}
	var latest float64
	if err := d.SQL.QueryRow("SELECT coalesce(max(coalesce(finished_at,created_at)),0) FROM jobs WHERE trigger<>'incremental'").Scan(&latest); err != nil {
		return false, err
	}
	last = max(last, latest)
	return last == 0 || float64(now.UnixNano())/1e9-last >= float64(c.IntervalMinutes*60), nil
}

// Called while idle under the manager lock. Directory jobs belong to their
// full scan and are removed with it; they do not consume retention slots.
func (m *Manager) pruneLocked(keep int) error {
	if keep == 0 {
		return nil
	}
	items, err := platform.Rows(m.db.SQL, `SELECT id FROM (SELECT id,status,analysis_status FROM jobs
 WHERE trigger<>'incremental' AND status NOT IN ('queued','running','cancelling')
 ORDER BY created_at DESC,id DESC LIMIT -1 OFFSET ?) WHERE NOT (status='completed' AND analysis_status='pending')`, keep)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err = m.deleteLocked(item["id"].(string), "scheduler"); err != nil {
			return fmt.Errorf("清理过期扫描记录: %w", err)
		}
	}
	return nil
}
