package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Save the rendered report and verified findings in one transaction.
func (a *Manager) saveReport(id, text string, subjects []reportSubject, results []reportResult) error {
	rows, _ := mergeReportFindings(subjects, results)
	return a.db.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("INSERT INTO agent_messages(session_id,role,content,created_at) VALUES(?,'assistant',?,?)", id, text, platform.Now())
		if err != nil {
			return err
		}
		messageID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO agent_reports(message_id,entries) VALUES(?,?)", messageID, httpapi.JSONText(rows))
		return err
	})
}

func (a *Manager) cleanupReports(userID string) (object, error) {
	rows, err := platform.Rows(a.db.SQL, `SELECT r.message_id AS report_id,s.title,s.snapshot_id,s.report_scope,m.created_at,
 c.id AS cleanup_id,c.status AS cleanup_status FROM agent_reports r
 JOIN agent_messages m ON m.id=r.message_id JOIN agent_sessions s ON s.id=m.session_id
 LEFT JOIN agent_cleanups c ON c.report_id=r.message_id
 WHERE s.user_id=? AND s.node_id=? ORDER BY m.id DESC`, userID, a.db.NodeID)
	return object{"reports": rows}, err
}

func (a *Manager) prepareCleanup(reportID int64, userID, actor string) (object, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, httpapi.NewError(503, "服务正在关闭")
	}
	if err := a.authorized(userID); err != nil {
		return nil, err
	}
	var manifestText string
	err := a.db.SQL.QueryRow(`SELECT r.entries FROM agent_reports r
 JOIN agent_messages m ON m.id=r.message_id JOIN agent_sessions s ON s.id=m.session_id
 WHERE r.message_id=? AND s.user_id=? AND s.node_id=?`, reportID, userID, a.db.NodeID).Scan(&manifestText)
	if err == sql.ErrNoRows {
		return nil, httpapi.NewError(404, "完整报告不存在")
	}
	if err != nil {
		return nil, err
	}
	var id string
	err = a.db.SQL.QueryRow("SELECT id FROM agent_cleanups WHERE report_id=?", reportID).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if id != "" {
		return a.cleanup(id, userID)
	}
	var findings []*mergedReportFinding
	if err = json.Unmarshal([]byte(manifestText), &findings); err != nil {
		return nil, fmt.Errorf("读取报告条目: %w", err)
	}
	id = platform.RandomHex(16)
	err = a.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO agent_cleanups(id,report_id,status,created_at,updated_at) VALUES(?,?,'ready',?,?)", id, reportID, platform.Now(), platform.Now()); err != nil {
			return err
		}
		for _, row := range findings {
			if _, err := tx.Exec("INSERT INTO agent_cleanup_entries(id,cleanup_id,path,category,summary,detail) VALUES(?,?,?,?,?,?)", platform.RandomHex(16), id, row.Path, row.Category, row.Summary, httpapi.JSONText(row)); err != nil {
				return err
			}
		}
		return platform.Audit(tx, actor, "storage.cleanup.prepare", id)
	})
	if err != nil {
		return nil, err
	}
	return a.cleanup(id, userID)
}

func (a *Manager) cleanup(id, userID string) (object, error) {
	records, err := platform.Rows(a.db.SQL, `SELECT c.*,s.snapshot_id,s.report_scope FROM agent_cleanups c
 JOIN agent_reports r ON r.message_id=c.report_id JOIN agent_messages m ON m.id=r.message_id
 JOIN agent_sessions s ON s.id=m.session_id WHERE c.id=? AND s.user_id=? AND s.node_id=?`, id, userID, a.db.NodeID)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, httpapi.NewError(404, "清理记录不存在")
	}
	rows, err := platform.Rows(a.db.SQL, "SELECT id,path,category,summary,detail,status,error FROM agent_cleanup_entries WHERE cleanup_id=? ORDER BY category,path", id)
	return object{"cleanup": records[0], "entries": rows}, err
}

func (a *Manager) removeCleanup(id, userID, actor string) (object, error) {
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
	current, err := a.cleanup(id, userID)
	if err != nil {
		return nil, err
	}
	status := current["cleanup"].(object)["status"]
	if a.active == id || status == "running" || status == "cancelling" {
		return nil, httpapi.NewError(409, "删除仍在执行，请先停止并等待任务结束")
	}
	err = a.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM agent_cleanups WHERE id=?", id); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "storage.cleanup.remove", id)
	})
	return object{"ok": true}, err
}

func (a *Manager) stopCleanup(id, userID string) (object, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.flushCompletionLocked(); err != nil {
		return nil, err
	}
	if _, err := a.cleanup(id, userID); err != nil {
		return nil, err
	}
	if a.active != id || a.cancel == nil {
		return nil, httpapi.NewError(409, "删除已结束")
	}
	if _, err := a.db.SQL.Exec("UPDATE agent_cleanups SET status='cancelling',updated_at=? WHERE id=?", platform.Now(), id); err != nil {
		return nil, err
	}
	a.cancel()
	return object{"ok": true}, nil
}

// This capability is deliberately absent from model tools. Only authenticated
// browser selections of persisted entry IDs can invoke it.
type cleanupStorage interface {
	DeleteReportPaths(context.Context, string, string, []string, []byte, func(string, string, string) error) error
}

type hostCleanupStorage interface {
	DeleteHostReportPaths(context.Context, string, string, []string, []byte, func(string, string, string) error) error
}

func (a *Manager) deleteCleanup(id, userID, actor string, ids []string, password []byte) (object, error) {
	transferred := false
	defer func() {
		if !transferred {
			clear(password)
		}
	}()
	if len(password) == 0 || len(password) > 1024 || bytes.IndexAny(password, "\x00\r\n") >= 0 {
		return nil, httpapi.NewError(400, "请输入本次删除使用的 sudo 密码（不超过 1024 字节，不能包含换行或 NUL）")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, httpapi.NewError(503, "服务正在关闭")
	}
	if err := a.flushCompletionLocked(); err != nil {
		return nil, err
	}
	if a.active != "" {
		return nil, httpapi.NewError(409, "当前节点已有任务正在执行，请等待完成后再删除")
	}
	if err := a.authorized(userID); err != nil {
		return nil, err
	}
	current, err := a.cleanup(id, userID)
	if err != nil {
		return nil, err
	}
	cleanup := current["cleanup"].(object)
	if httpapi.String(cleanup["snapshot_id"]) == "" {
		return nil, httpapi.NewError(409, "源扫描已删除，请重新扫描并生成报告")
	}
	if len(ids) == 0 || len(ids) > 100 {
		return nil, httpapi.NewError(400, "每次请选择 1–100 个条目")
	}
	rows := current["entries"].([]object)
	byID := map[string]object{}
	for _, row := range rows {
		byID[httpapi.String(row["id"])] = row
	}
	paths := []string{}
	selected := map[string]string{}
	for _, entryID := range ids {
		row, ok := byID[entryID]
		if !ok || (row["status"] != "pending" && row["status"] != "failed") {
			return nil, httpapi.NewError(409, "条目不存在或已处理，请刷新列表")
		}
		p := httpapi.String(row["path"])
		if selected[p] != "" {
			return nil, httpapi.NewError(400, "条目重复")
		}
		selected[p] = entryID
		paths = append(paths, p)
	}
	for _, p := range paths {
		for _, other := range paths {
			if p != other && strings.HasPrefix(p, strings.TrimRight(other, "/")+"/") {
				return nil, httpapi.NewError(400, "所选条目包含父子目录，请仅选择其中一项")
			}
		}
	}
	service, ok := a.records.(cleanupStorage)
	if !ok {
		return nil, httpapi.NewError(503, "存储服务不支持目录删除")
	}
	deletePaths := service.DeleteReportPaths
	if httpapi.String(cleanup["report_scope"]) == "host" {
		hostService, ok := a.records.(hostCleanupStorage)
		if !ok {
			return nil, httpapi.NewError(503, "存储服务不支持 Host 路径归属复查")
		}
		deletePaths = hostService.DeleteHostReportPaths
	}
	// A durable intent precedes all filesystem changes. An interrupted operation
	// remains explicit and cannot silently be submitted again after a restart.
	err = a.db.Transaction(func(tx *sql.Tx) error {
		for _, entryID := range ids {
			if _, err := tx.Exec("UPDATE agent_cleanup_entries SET status='deleting',error='' WHERE id=?", entryID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE agent_cleanups SET status='running',error='',updated_at=? WHERE id=?", platform.Now(), id); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "storage.cleanup", httpapi.JSONText(object{"cleanup_id": id, "paths": paths}))
	})
	if err != nil {
		return nil, err
	}
	a.launchLocked(id, true, func(ctx context.Context) error {
		defer clear(password)
		err := deletePaths(ctx, actor, httpapi.String(cleanup["snapshot_id"]), paths, password, func(p, status, message string) error {
			_, err := a.db.SQL.Exec("UPDATE agent_cleanup_entries SET status=?,error=? WHERE id=?", status, message, selected[p])
			return err
		})
		if err != nil {
			// Typed preflight failures made no filesystem changes. Other failures may
			// have interrupted publication of a deletion result and remain uncertain.
			status, message := "uncertain", "操作中断，请检查实际路径后重新扫描："+err.Error()
			var apiErr *httpapi.Error
			if errors.As(err, &apiErr) {
				status, message = "failed", err.Error()
			}
			_, saveErr := a.db.SQL.Exec("UPDATE agent_cleanup_entries SET status=?,error=? WHERE cleanup_id=? AND status='deleting'", status, message, id)
			if saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	})
	transferred = true
	return a.cleanup(id, userID)
}
