package storage

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/httpapi"
)

type object = map[string]any

type Config struct {
	AutoAgentAnalyze  bool     `json:"auto_agent_analyze"`
	Root              []string `json:"root"`
	Exclude           []string `json:"exclude"`
	NoDocker          bool     `json:"no_docker"`
	IncludeDockerRoot bool     `json:"include_docker_root"`
	MaxDepth          int      `json:"max_depth"`
	MaxNodes          int      `json:"max_nodes"`
	OwnerLabel        string   `json:"owner_label"`
	DockerTimeout     int      `json:"docker_timeout"`
	IntervalMinutes   int      `json:"interval_minutes"`
	ScheduleMode      string   `json:"schedule_mode"`
	ScheduleTimes     []string `json:"schedule_times"`
	ScheduleWeekdays  []int    `json:"schedule_weekdays"`
	ScheduleTimezone  string   `json:"schedule_timezone"`
	RetainRecords     int      `json:"retain_records"`
	ScanBackend       string   `json:"scan_backend"`
	ScanMode          string   `json:"scan_mode"`
}

func defaultConfig() Config {
	return Config{Root: []string{}, Exclude: []string{}, MaxDepth: 5, MaxNodes: 50000, OwnerLabel: "project-alpha.owner", DockerTimeout: 120, ScanBackend: "auto", ScanMode: "normal", ScheduleMode: "off", ScheduleTimes: []string{}, ScheduleWeekdays: []int{0, 1, 2, 3, 4, 5, 6}, ScheduleTimezone: "UTC"}
}

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var containerPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func parseConfig(raw json.RawMessage) (Config, error) {
	var fields map[string]json.RawMessage
	var c Config
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 17 {
		return c, httpapi.NewError(400, "扫描配置字段不完整或包含未知字段")
	}
	for _, key := range []string{"auto_agent_analyze", "root", "exclude", "no_docker", "include_docker_root", "max_depth", "max_nodes", "owner_label", "docker_timeout", "interval_minutes", "scan_backend", "scan_mode", "schedule_mode", "schedule_times", "schedule_weekdays", "schedule_timezone", "retain_records"} {
		if v, ok := fields[key]; !ok || string(v) == "null" {
			return c, httpapi.NewError(400, "扫描配置字段不完整或包含未知字段")
		}
	}
	if json.Unmarshal(raw, &c) != nil {
		return c, httpapi.NewError(400, "扫描配置字段类型无效")
	}
	return c, c.validate()
}

const maxScanNodes = 100000

func (c Config) validate() error {
	if err := c.validateSchedule(); err != nil {
		return err
	}
	if c.ScanMode != "normal" && c.ScanMode != "fast" {
		return httpapi.NewError(400, "扫描模式必须是 normal 或 fast")
	}
	if c.ScanBackend != "auto" && c.ScanBackend != "host" && c.ScanBackend != "docker" {
		return httpapi.NewError(400, "扫描方式必须是 auto、host 或 docker")
	}
	if c.NoDocker && c.ScanBackend == "docker" {
		return httpapi.NewError(400, "Docker 辅助扫描需要启用 Docker 自动发现")
	}
	for _, paths := range [][]string{c.Root, c.Exclude} {
		if len(paths) > 100 {
			return httpapi.NewError(400, "目录列表最多允许 100 项")
		}
		for _, p := range paths {
			if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || utf8.RuneCountInString(p) > 4096 {
				return httpapi.NewError(400, "目录必须是有效的宿主机绝对路径")
			}
		}
	}
	for _, r := range []struct {
		name             string
		value, low, high int
	}{{"max_depth", c.MaxDepth, 0, 32}, {"max_nodes", c.MaxNodes, 100, maxScanNodes}, {"docker_timeout", c.DockerTimeout, 5, 3600}, {"interval_minutes", c.IntervalMinutes, 0, 10080}} {
		if r.value < r.low || r.value > r.high {
			return httpapi.NewError(400, fmt.Sprintf("%s 必须在 %d–%d 之间", r.name, r.low, r.high))
		}
	}
	if c.IntervalMinutes > 0 && c.IntervalMinutes < 5 {
		return httpapi.NewError(400, "定时扫描间隔至少为 5 分钟")
	}
	if c.NoDocker && len(c.Root) == 0 {
		return httpapi.NewError(400, "关闭 Docker 发现时必须设置至少一个扫描目录")
	}
	if !labelPattern.MatchString(c.OwnerLabel) {
		return httpapi.NewError(400, "容器用户标签格式无效")
	}
	return nil
}
