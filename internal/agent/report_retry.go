package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type savedReportGroup struct {
	ID          string            `json:"id"`
	Number      int               `json:"number"`
	Containers  []reportSubject   `json:"containers,omitempty"`
	Directories []reportDirectory `json:"directories,omitempty"`
}

type savedReport struct {
	Groups      []savedReportGroup `json:"groups"`
	Concurrency int                `json:"concurrency"`
	Scope       string             `json:"scope"`
	States      map[string]string
}

type reportRetry struct {
	GroupID   string `json:"group_id"`
	RequestID string `json:"request_id"`
}

type reportRetryTask struct {
	target reportRetry
	group  savedReportGroup
	resume *reportResume
}

type reportResume struct {
	Round     int
	History   []object
	Inspected map[string]bool
}

func (a *Manager) savedReport(id string) (*savedReport, error) {
	var raw string
	if err := a.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='report_plan' ORDER BY id DESC LIMIT 1", id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, httpapi.NewError(409, "此会话没有可重试的分组报告")
		}
		return nil, err
	}
	var report savedReport
	if json.Unmarshal([]byte(raw), &report) != nil || len(report.Groups) == 0 || report.Concurrency < 1 || report.Concurrency > maxReportConcurrency || (report.Scope != "host" && report.Scope != "container") {
		return nil, httpapi.NewError(409, "报告分组记录无效，无法恢复")
	}
	report.States = map[string]string{}
	for i, group := range report.Groups {
		if group.Number != i+1 || group.ID != fmt.Sprintf("group-%d", i+1) || (report.Scope == "container" && (len(group.Containers) == 0 || len(group.Directories) != 0)) || (report.Scope == "host" && (len(group.Directories) == 0 || len(group.Containers) != 0)) {
			return nil, httpapi.NewError(409, "报告分组记录无效，无法恢复")
		}
		report.States[group.ID] = "queued"
	}
	rows, err := platform.Rows(a.db.SQL, "SELECT content FROM agent_messages WHERE session_id=? AND role='group_state' ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var state struct{ ID, Status string }
		if json.Unmarshal([]byte(httpapi.String(row["content"])), &state) != nil || report.States[state.ID] == "" {
			return nil, httpapi.NewError(409, "Agent 状态记录无效，无法恢复")
		}
		report.States[state.ID] = state.Status
	}
	return &report, nil
}

// Restore the input of the latest failed request, never its partial output.
// Completed tool observations in that input are reused without executing tools.
func (a *Manager) reportResume(id string, group savedReportGroup, requestID string, c Config) (*reportResume, error) {
	var raw string
	var messageID int64
	err := a.db.SQL.QueryRow(`SELECT id,content FROM agent_messages WHERE session_id=? AND role='model_request'
 AND json_extract(content,'$.group_id')=? ORDER BY id DESC LIMIT 1`, id, group.ID).Scan(&messageID, &raw)
	if err == sql.ErrNoRows {
		return nil, httpapi.NewError(409, "此 Agent 没有可恢复的模型请求")
	}
	if err != nil {
		return nil, err
	}
	var request struct {
		RequestID string   `json:"request_id"`
		Protocol  string   `json:"protocol"`
		Round     int      `json:"round"`
		Context   []object `json:"context"`
	}
	if json.Unmarshal([]byte(raw), &request) != nil || request.Round < 1 || len(request.Context) == 0 {
		return nil, httpapi.NewError(409, "失败请求缺少完整上下文，无法从失败处继续")
	}
	if request.RequestID != requestID {
		return nil, httpapi.NewError(409, "失败请求已更新，请刷新后重试最近一次失败")
	}
	if request.Protocol != c.Protocol {
		return nil, httpapi.NewError(409, "接口类型已变更，请恢复为 "+request.Protocol+" 后重试，以保留原请求上下文")
	}
	var status string
	err = a.db.SQL.QueryRow(`SELECT json_extract(content,'$.status') FROM agent_messages
 WHERE session_id=? AND role='model_response' AND json_extract(content,'$.request_id')=?
 AND json_extract(content,'$.group_id')=? ORDER BY id DESC LIMIT 1`, id, requestID, group.ID).Scan(&status)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if status != "failed" {
		return nil, httpapi.NewError(409, "最近一次模型请求未失败，不能重复发送")
	}
	resume := &reportResume{Round: request.Round, Inspected: map[string]bool{}}
	for _, item := range request.Context {
		// Responses opaque reasoning is intentionally never persisted. Public
		// messages, calls and outputs are sufficient to resume a saved request.
		if item["type"] != "reasoning" {
			resume.History = append(resume.History, item)
		}
	}
	if len(resume.History) == 0 {
		return nil, httpapi.NewError(409, "失败请求没有可恢复的消息上下文")
	}
	queryTool := "get_container"
	if len(group.Directories) > 0 {
		queryTool = "get_host_directory"
	}
	secondTool := queryTool
	if queryTool == "get_host_directory" {
		secondTool = "scan_directory"
	}
	rows, err := platform.Rows(a.db.SQL, `SELECT role,content,tool_name FROM agent_messages WHERE session_id=? AND id<?
 AND role IN ('tool_start','tool_end') AND tool_name IN (?,?)
 AND json_extract(content,'$.group_id')=? ORDER BY id`, id, messageID, queryTool, secondTool, group.ID)
	if err != nil {
		return nil, err
	}
	calls := map[string]string{}
	for _, row := range rows {
		var event struct {
			CallID            string `json:"call_id"`
			Arguments, Status string
		}
		if json.Unmarshal([]byte(httpapi.String(row["content"])), &event) != nil {
			return nil, httpapi.NewError(409, "工具记录无效，无法恢复已完成的查询")
		}
		if row["role"] == "tool_start" {
			var args struct{ Container, Path string }
			if json.Unmarshal([]byte(event.Arguments), &args) != nil {
				return nil, httpapi.NewError(409, "工具参数记录无效")
			}
			if queryTool == "get_host_directory" {
				calls[event.CallID] = args.Path
			} else {
				calls[event.CallID] = args.Container
			}
		} else if target, ok := calls[event.CallID]; ok {
			for _, directory := range group.Directories {
				if target == directory.Path {
					resume.Inspected[directory.Path] = event.Status == "completed"
				}
			}
			for _, container := range group.Containers {
				if target == container.ID || target == container.Name {
					resume.Inspected[container.ID] = event.Status == "completed"
				}
			}
		}
	}
	return resume, nil
}

func (a *Manager) retryGroups(id, userID, actor string, targets []reportRetry) (object, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, httpapi.NewError(503, "服务正在关闭")
	}
	if err := a.flushCompletionLocked(); err != nil {
		return nil, err
	}
	if err := a.authorized(userID); err != nil {
		return nil, err
	}
	session, err := a.session(id, userID)
	if err != nil {
		return nil, err
	}
	if a.active != "" {
		return nil, httpapi.NewError(409, "当前节点已有任务正在执行，请等待完成后重试")
	}
	snapshotID := httpapi.String(session["snapshot_id"])
	if snapshotID == "" {
		return nil, httpapi.NewError(409, "分析所需的扫描记录已删除，无法重试")
	}
	if _, err = a.records.Job(snapshotID); err != nil {
		return nil, err
	}
	report, err := a.savedReport(id)
	if err != nil {
		return nil, err
	}
	if httpapi.String(session["report_scope"]) != report.Scope {
		return nil, httpapi.NewError(409, "报告范围记录无效，无法重试")
	}
	groups := map[string]savedReportGroup{}
	for _, candidate := range report.Groups {
		groups[candidate.ID] = candidate
	}
	c, _, err := a.db.agentConfig()
	if err != nil {
		return nil, err
	}
	if err = c.validate(); err != nil {
		return nil, err
	}
	if c.Model == "" {
		return nil, httpapi.NewError(400, "请先配置模型名称和接口")
	}
	if len(targets) == 0 || len(targets) > len(report.Groups) {
		return nil, httpapi.NewError(400, "需要有效的失败 Agent 请求列表")
	}
	tasks := make([]reportRetryTask, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		if target.GroupID == "" || target.RequestID == "" || seen[target.GroupID] {
			return nil, httpapi.NewError(400, "重试请求缺少标识或包含重复 Agent")
		}
		seen[target.GroupID] = true
		if report.States[target.GroupID] != "failed" {
			return nil, httpapi.NewError(409, "只能重试失败的 Agent，请刷新记录")
		}
		group := groups[target.GroupID]
		resume, err := a.reportResume(id, group, target.RequestID, c)
		if err != nil {
			return nil, fmt.Errorf("Agent %d：%w", group.Number, err)
		}
		tasks = append(tasks, reportRetryTask{target, group, resume})
	}
	err = a.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE agent_sessions SET status='running',error=NULL,updated_at=?,provider=?,model=? WHERE id=?", platform.Now(), c.Protocol, c.Model, id); err != nil {
			return err
		}
		for _, task := range tasks {
			notice := object{"group_id": task.target.GroupID, "request_id": task.target.RequestID, "text": fmt.Sprintf("已加入重试队列，将从第 %d 轮失败请求继续分析，保留此前的工具结果。", task.resume.Round)}
			for role, event := range map[string]object{"group_status": notice, "group_state": {"id": task.target.GroupID, "status": "queued"}} {
				if _, err := tx.Exec("INSERT INTO agent_messages(session_id,role,content,created_at) VALUES(?,?,?,?)", id, role, httpapi.JSONText(event), platform.Now()); err != nil {
					return err
				}
			}
			if err := platform.Audit(tx, actor, "agent.retry", id+":"+task.target.GroupID+":"+task.target.RequestID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.launchLocked(id, false, func(ctx context.Context) error {
		if err := a.runReportRetries(ctx, id, userID, actor, c, snapshotID, report, tasks); err != nil {
			return err
		}
		return a.finishReport(ctx, id, snapshotID)
	})
	return a.session(id, userID)
}

func (a *Manager) runReportRetries(ctx context.Context, id, userID, actor string, c Config, snapshotID string, report *savedReport, tasks []reportRetryTask) error {
	err := runReportWorkers(ctx, len(tasks), report.Concurrency, func(index int) error {
		task := tasks[index]
		return a.runReportGroup(ctx, id, userID, actor, c, snapshotID, report.Scope, task.group, len(report.Groups), task.resume)
	})
	if ctx.Err() != nil {
		state, readErr := a.savedReport(id)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		for _, task := range tasks {
			if state.States[task.group.ID] == "queued" {
				err = errors.Join(err, a.message(id, "group_state", httpapi.JSONText(object{"id": task.group.ID, "status": "failed", "error": "重试尚未开始便已停止，可再次重试原失败请求"}), ""))
			}
		}
	}
	return err
}

func (a *Manager) finishReport(ctx context.Context, id, snapshotID string) error {
	report, err := a.savedReport(id)
	if err != nil {
		return err
	}
	subjects := []reportSubject{}
	byGroup := map[string]savedReportGroup{}
	for _, group := range report.Groups {
		if report.States[group.ID] != "completed" {
			return fmt.Errorf("Agent %d 尚未完成；已完成结果保留，可继续重试失败的 Agent", group.Number)
		}
		if report.Scope == "host" {
			subjects = append(subjects, hostReportSubjects(group.Directories)...)
		} else {
			subjects = append(subjects, group.Containers...)
		}
		byGroup[group.ID] = group
	}
	// Keep original publication order, so merging shared paths retains the same
	// latest-observation semantics as the initial concurrent report run.
	rows, err := platform.Rows(a.db.SQL, "SELECT id,content FROM agent_messages WHERE session_id=? AND role='group_report' ORDER BY id", id)
	if err != nil {
		return err
	}
	results := []reportResult{}
	for _, row := range rows {
		var event struct {
			GroupID string `json:"group_id"`
		}
		if json.Unmarshal([]byte(httpapi.String(row["content"])), &event) != nil || byGroup[event.GroupID].ID == "" {
			return httpapi.NewError(409, "分组结果记录无效，无法合并报告")
		}
		var raw string
		if err := a.db.SQL.QueryRow(`SELECT json_extract(content,'$.text') FROM agent_messages
 WHERE session_id=? AND id<? AND role='model_response' AND json_extract(content,'$.group_id')=?
 AND json_extract(content,'$.status')='completed' ORDER BY id DESC LIMIT 1`, id, row["id"], event.GroupID).Scan(&raw); err != nil {
			return err
		}
		if report.Scope == "host" {
			parsed, err := parseHostReportGroup(raw, byGroup[event.GroupID].Directories)
			if err != nil {
				return err
			}
			results = append(results, hostReportResults(parsed.Directories)...)
		} else {
			parsed, err := parseReportGroup(raw, byGroup[event.GroupID].Containers)
			if err != nil {
				return err
			}
			results = append(results, parsed.Containers...)
		}
		delete(byGroup, event.GroupID)
	}
	if len(byGroup) != 0 {
		return httpapi.NewError(409, "缺少已完成分组的结果，无法合并报告")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := a.records.Query(snapshotID, "overview", nil)
	if err != nil {
		return err
	}
	location := "容器 / 内部路径"
	coverageText := ""
	header := fmt.Sprintf("# 空间消耗总报告\n\n%d 个容器 · %d 组 · 同一物理路径已合并，条目不相加为可回收总量。\n\n", len(subjects), len(report.Groups))
	if report.Scope == "host" {
		header = fmt.Sprintf("# Host 空间分析报告\n\n%d 个 Host 目录 · %d 组 · 仅统计未被容器引用的路径，条目不相加为可回收总量。\n\n", len(subjects), len(report.Groups))
		location = "Host 区域"
		directories := []reportDirectory{}
		for _, group := range report.Groups {
			directories = append(directories, group.Directories...)
		}
		coverage, err := a.hostReportCoverage(ctx, snapshotID, directories, results)
		if err != nil {
			return err
		}
		coverageText = renderHostCoverage(coverage)
	}
	footer := fmt.Sprintf("\n记录：%s · 版本 %v · 基线 %v · 更新 %v。\n", snapshotID, current["revision"], current["observed_at"], current["updated_at"])
	return a.saveReport(id, header+renderReportFindings(subjects, results, location)+coverageText+footer, subjects, results)
}
