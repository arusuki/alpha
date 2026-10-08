package storage

import (
	"encoding/json"

	"project-alpha/internal/httpapi"
)

// Schedule contains only the settings shared when distributing a scan plan.
type Schedule struct {
	Mode            string   `json:"schedule_mode"`
	IntervalMinutes int      `json:"interval_minutes"`
	Times           []string `json:"schedule_times"`
	Weekdays        []int    `json:"schedule_weekdays"`
	Timezone        string   `json:"schedule_timezone"`
	RetainRecords   int      `json:"retain_records"`
}

func ParseSchedule(raw json.RawMessage) (Schedule, error) {
	var s Schedule
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 6 {
		return s, httpapi.NewError(400, "扫描计划字段不完整或包含未知字段")
	}
	for _, key := range []string{"schedule_mode", "interval_minutes", "schedule_times", "schedule_weekdays", "schedule_timezone", "retain_records"} {
		if v, ok := fields[key]; !ok || string(v) == "null" {
			return s, httpapi.NewError(400, "扫描计划字段不完整或包含未知字段")
		}
	}
	if json.Unmarshal(raw, &s) != nil {
		return s, httpapi.NewError(400, "扫描计划字段类型无效")
	}
	c := defaultConfig()
	s.Apply(&c)
	return s, c.validate()
}

func (s Schedule) Apply(c *Config) {
	c.ScheduleMode, c.IntervalMinutes = s.Mode, s.IntervalMinutes
	c.ScheduleTimes, c.ScheduleWeekdays = s.Times, s.Weekdays
	c.ScheduleTimezone, c.RetainRecords = s.Timezone, s.RetainRecords
}
