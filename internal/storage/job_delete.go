package storage

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Delete removes a record and its directory tasks under the same lock used to
// start workers. Published directory results cannot be removed independently.

func (m *Manager) Delete(id, actor string) (object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reapLocked(); err != nil {
		return nil, err
	}
	deleted := []string{}
	err := m.db.Transaction(func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow("SELECT status FROM jobs WHERE id=?", id).Scan(&status); err == sql.ErrNoRows {
			return httpapi.NewError(404, "扫描任务不存在")
		} else if err != nil {
			return err
		}
		items, err := platform.Rows(tx, "SELECT id,status FROM jobs WHERE id=? OR json_extract(config,'$.base_job_id')=?", id, id)
		if err != nil {
			return err
		}
		for _, item := range items {
			jobID := item["id"].(string)
			if activeStatus(item["status"].(string)) || m.process != nil && m.jobID == jobID {
				return httpapi.NewError(409, "该记录或关联目录任务仍在运行，请等待结束或取消任务后再删除")
			}
			// IDs also become directory names. Never accept paths from stored data.
			if !jobRoute.MatchString("/api/jobs/"+jobID) || len(jobID) != 32 {
				return httpapi.NewError(500, "扫描记录标识无效")
			}
			deleted = append(deleted, jobID)
			var analyzing int
			if err := tx.QueryRow(`SELECT count(*) FROM agent_sessions s
			 WHERE status IN ('queued','scanning','running','cancelling') AND
			 (snapshot_id=? OR active_job_id=? OR EXISTS
			 (SELECT 1 FROM agent_details d WHERE d.session_id=s.id AND d.job_id=?))`, jobID, jobID, jobID).Scan(&analyzing); err != nil {
				return err
			}
			if analyzing > 0 {
				return httpapi.NewError(409, "该记录正在用于 Agent 分析，请等待分析结束或停止分析后再删除")
			}
		}
		for _, jobID := range deleted {
			for _, statement := range []string{
				"DELETE FROM agent_details WHERE job_id=?",
				"UPDATE agent_sessions SET snapshot_id=NULL WHERE snapshot_id=?",
				"UPDATE agent_sessions SET active_job_id=NULL WHERE active_job_id=?",
			} {
				if _, err := tx.Exec(statement, jobID); err != nil {
					return err
				}
			}
		}
		for _, jobID := range deleted {
			if _, err := tx.Exec("DELETE FROM jobs WHERE id=?", jobID); err != nil {
				return err
			}
		}
		return platform.Audit(tx, actor, "scan.delete", httpapi.JSONText(object{"job_id": id, "deleted_ids": deleted}))
	})
	if err != nil {
		return nil, err
	}
	// Commit first: a database failure must leave every snapshot readable.
	cleanupPending := false
	for _, jobID := range deleted {
		if err := os.RemoveAll(filepath.Join(m.db.Directory, "results", jobID)); err != nil {
			log.Printf("remove deleted scan result %s: %v", jobID, err)
			cleanupPending = true
		}
	}
	return object{"deleted_ids": deleted, "cleanup_pending": cleanupPending}, nil
}
