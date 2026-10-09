package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
)

// AdvanceScheduled reuses durable sessions and cleanup manifests after reconnects.
// Admission must be serialized with other node operations by the caller.
// done=false leaves the worker record protected until another queue pass.
func (a *Manager) AdvanceScheduled(snapshotID, userID, actor string) (done bool, result error) {
	if err := a.authorized(userID); err != nil {
		return true, err
	}
	if err := a.flushCompletion(); err != nil {
		return false, err
	}
	failures := []string{}
	for _, scope := range []string{"host", "container"} {
		var id, status, message string
		err := a.db.SQL.QueryRow("SELECT id,status,coalesce(error,'') FROM agent_sessions WHERE node_id=? AND scheduled_job_id=? AND report_scope=?", a.db.NodeID, snapshotID, scope).Scan(&id, &status, &message)
		if err == sql.ErrNoRows {
			job, err := a.records.Job(snapshotID)
			if err != nil {
				return false, err
			}
			revision, err := strconv.ParseInt(fmt.Sprint(job["snapshot_revision"]), 10, 64)
			if err != nil {
				return true, fmt.Errorf("扫描记录版本无效: %w", err)
			}
			request := hostReportRequest
			if scope == "container" {
				request = diskReportRequest
			}
			_, err = a.start("", userID, actor, request, &reportSource{SnapshotID: snapshotID, Revision: revision, Scope: scope, Concurrency: 4, Scheduled: true})
			var apiErr *httpapi.Error
			if errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 403) {
				return true, err
			}
			return false, err
		}
		if err != nil {
			return false, err
		}
		switch status {
		case "queued", "running", "scanning", "cancelling":
			return false, nil
		case "cancelled", "interrupted":
			// A stop or restart must not silently launch the next report scope.
			return true, fmt.Errorf("%s：%s %s", scope, status, message)
		case "completed":
			var reportID int64
			if err := a.db.SQL.QueryRow("SELECT r.message_id FROM agent_reports r JOIN agent_messages m ON m.id=r.message_id WHERE m.session_id=? ORDER BY r.message_id DESC LIMIT 1", id).Scan(&reportID); err != nil {
				if err == sql.ErrNoRows {
					failures = append(failures, scope+"：未生成完整报告")
					continue
				}
				return false, err
			}
			if _, err := a.prepareCleanup(reportID, userID, actor); err != nil {
				return false, err
			}
		default:
			failures = append(failures, scope+"："+status+" "+message)
		}
	}
	if len(failures) > 0 {
		return true, errors.New(strings.Join(failures, "；"))
	}
	return true, nil
}
