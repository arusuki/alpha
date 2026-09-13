package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (d *Store) readSnapshot(id string) (*Snapshot, error) {
	var status string
	err := d.SQL.QueryRow("SELECT status FROM jobs WHERE id=?", id).Scan(&status)
	if err == sql.ErrNoRows {
		return nil, httpapi.NewError(404, "扫描任务不存在")
	}
	if err != nil {
		return nil, err
	}
	if status != "completed" {
		return nil, httpapi.NewError(409, "该任务尚未生成完整结果")
	}
	if result, err := d.storedSnapshot(id); err != sql.ErrNoRows {
		return result, err
	}
	raw, err := os.ReadFile(filepath.Join(d.Directory, "results", id, "snapshot.json"))
	if err != nil {
		return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
	}
	var result Snapshot
	if json.Unmarshal(raw, &result) != nil || result.SchemaVersion != snapshotVersion || result.Tree == nil {
		return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
	}
	result.JobID = id
	return &result, nil
}

// StartIncremental queues a directory scan using the existing worker lock.
func (m *Manager) StartIncremental(actor, id, path string, revision int64, depth int) (object, error) {
	if err := validateIncrementalDepth(depth); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.db.job(id)
	if err != nil {
		return nil, err
	}
	if job["trigger"] == "agent-detail" || job["trigger"] == "incremental" {
		return nil, httpapi.NewError(400, "请在原扫描记录上继续分析")
	}
	base, err := m.db.readSnapshot(id)
	if err != nil {
		return nil, err
	}
	if base.Revision != revision {
		return nil, httpapi.NewError(409, "扫描记录已更新，请刷新结果后重试")
	}
	if err = validateIncrementalTarget(base, path); err != nil {
		return nil, err
	}
	var plan scanPlan
	if err = json.Unmarshal([]byte(httpapi.JSONText(job["config"])), &plan); err != nil {
		return nil, err
	}
	if _, err = validateDetailPath(path, plan.Config, m.db.Directory); err != nil {
		return nil, httpapi.NewError(400, err.Error())
	}
	plan.AnalysisPath = ""
	plan.BaseJobID = id
	plan.BaseRevision = revision
	plan.IncrementalPath = path
	plan.Root = []string{path}
	plan.IncludeDockerRoot = false
	plan.MaxDepth = depth
	plan.MaxNodes = maxScanNodes
	return m.startPlanLocked(actor, "incremental", plan)
}

// Each observation is durable before its revision becomes visible. Cancellation
// retains already published observations and cannot publish an unfinished write.
func (d *Store) publishDirectory(ctx context.Context, id string, plan scanPlan, result *Snapshot, progress object, complete bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.Transaction(func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow("SELECT status FROM jobs WHERE id=?", id).Scan(&status); err != nil {
			return err
		}
		if status != "running" {
			return context.Canceled
		}
		at := platform.Now()
		if plan.BaseJobID != "" {
			var revision int64
			if err := tx.QueryRow("SELECT coalesce((SELECT revision FROM snapshot_records WHERE job_id=?),0)", plan.BaseJobID).Scan(&revision); err != nil {
				return err
			}
			if revision != result.Revision-1 || revision < plan.BaseRevision {
				return fmt.Errorf("扫描记录版本已变化，目录结果未发布")
			}
			if err := storeSnapshot(tx, plan.BaseJobID, plan.IncrementalPath, result); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO snapshot_changes(job_id,revision,path) VALUES(?,?,?) ON CONFLICT(job_id,path) DO UPDATE SET revision=excluded.revision", plan.BaseJobID, result.Revision, plan.IncrementalPath); err != nil {
				return err
			}
			// Keep the original task's scan context; a directory task's counters
			// are never the cumulative counters of the saved record.
			r, err := tx.Exec("UPDATE jobs SET allocated=?,files=?,warnings=?,progress=json_set(progress,'$.allocated',?,'$.record_allocated',?) WHERE id=? AND status='completed'", result.Tree.Allocated, result.Tree.Files, len(result.Warnings), result.Tree.Allocated, result.Tree.Allocated, plan.BaseJobID)
			if err != nil {
				return err
			}
			n, err := r.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("原扫描记录不可更新")
			}
		}
		if !complete {
			return nil
		}
		if _, err := tx.Exec("UPDATE jobs SET status='completed',finished_at=?,allocated=?,files=?,warnings=?,progress=? WHERE id=?", at, result.Tree.Allocated, result.Tree.Files, len(result.Warnings), httpapi.JSONText(progress), id); err != nil {
			return err
		}
		if plan.BaseJobID == "" {
			return nil
		}
		var actor string
		if err := tx.QueryRow("SELECT created_by FROM jobs WHERE id=?", id).Scan(&actor); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "scan.expand", plan.BaseJobID+" / "+plan.IncrementalPath+" / "+id)
	})
}
