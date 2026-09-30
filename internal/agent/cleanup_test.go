package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (r *testRecords) DeleteReportPaths(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
	if r.cleanup != nil {
		return r.cleanup(ctx, actor, id, paths, password, result)
	}
	return r.Service.DeleteReportPaths(ctx, actor, id, paths, password, result)
}

const testDeletePassword = "test-only-delete-password"

func TestCleanupUsesSavedFindingsAndDeletesSelectedPaths(t *testing.T) {
	protocol := "responses"
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	p.configure()
	var usedPassword []byte
	// The agent boundary is tested with a one-use credential consumer. Storage's
	// subprocess tests exercise the actual sudo protocol and deletion helper.
	p.records.cleanup = func(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
		usedPassword = password
		if string(password) != testDeletePassword {
			return httpapi.NewError(403, "sudo 认证失败，请重新输入密码")
		}
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err == nil && info.Mode()&os.ModeSymlink == 0 {
				err = os.RemoveAll(path)
			} else if err == nil {
				err = fmt.Errorf("symlink")
			}
			status, message := "deleted", ""
			if err != nil {
				status, message = "failed", "路径检查失败"
			}
			if err = result(path, status, message); err != nil {
				return err
			}
		}
		return nil
	}
	findings := []reportFinding{}
	for i, name := range []string{"cache", "uncertain", "keep", "misplaced"} {
		path := filepath.Join(p.storage, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(path, "data"), []byte("keep until selected"))
		findings = append(findings, reportFinding{Path: path, Category: i + 1, Kind: "其他", Summary: "用途", Reason: "处理条件"})
	}
	job := p.Expect(202, "POST", "/api/jobs", object{}, nil)
	snapshotID := job["id"].(string)
	waitJob(t, p.records, snapshotID)
	var userID string
	p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID)
	sourceID := platform.RandomHex(16)
	_, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,snapshot_id,provider,model,report_scope) VALUES(?,?,'空间消耗总报告','completed',1,1,?,?, 'test','container')", sourceID, userID, snapshotID, protocol)
	if err != nil {
		t.Fatal(err)
	}
	containers := []reportSubject{{ID: "container", Name: "name"}}
	results := []reportResult{{SubjectID: "container", Findings: findings}}
	fullReport := "# 空间报告\n" + renderReportFindings(containers, results, "容器 / 内部路径")
	if err = p.agent.saveReport(sourceID, fullReport, containers, results); err != nil {
		t.Fatal(err)
	}
	// Followup messages do not change the saved report findings.
	p.agent.message(sourceID, "assistant", "后续追问只提到一个目录", "")
	reports := p.Expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
	reportID := reports[0].(object)["report_id"]
	p.Expect(400, "POST", "/api/agent/cleanups", object{"report_id": reportID, "report": "override"}, nil)
	started := p.Expect(201, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
	id := started["cleanup"].(object)["id"].(string)
	if started["cleanup"].(object)["status"] != "ready" {
		t.Fatal(started)
	}
	sessions := p.Expect(200, "GET", "/api/agent/sessions", nil, nil)["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatal("cleanup created an analysis conversation", sessions)
	}
	state := p.Expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
	entries := state["entries"].([]any)
	if len(entries) != 4 {
		t.Fatal(state)
	}
	first := entries[0].(object)
	deleteURL := "/api/agent/cleanups/" + id + "/delete"
	// Reopening the report preserves cleanup outcomes.
	again := p.Expect(201, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
	if again["cleanup"].(object)["id"] != id {
		t.Fatal(again)
	}
	p.Expect(400, "POST", deleteURL, object{"paths": []string{p.storage}}, nil)
	p.Expect(400, "POST", deleteURL, object{"entry_ids": []any{first["id"]}}, nil)
	for _, password := range []any{"", nil, 123, "line\nbreak", strings.Repeat("x", 1025)} {
		p.Expect(400, "POST", deleteURL, object{"entry_ids": []any{first["id"]}, "sudo_password": password}, nil)
	}

	p.Expect(409, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []string{platform.RandomHex(16)}}, nil)
	p.Expect(400, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []string{first["id"].(string), first["id"].(string)}}, nil)
	p.Expect(403, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, map[string]string{"X-CSRF-Token": "wrong"})
	p.Expect(202, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, nil)
	waitCleanup(t, p, id)
	deleted := p.Expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
	if !bytes.Equal(usedPassword, make([]byte, len(testDeletePassword))) {
		t.Fatal("credential was not wiped after deletion")
	}
	if err := filepath.Walk(p.db.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(testDeletePassword)) {
			t.Error("credential persisted in platform data")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if deleted["entries"].([]any)[0].(object)["status"] != "deleted" {
		t.Fatal(deleted)
	}
	if _, err := os.Stat(findings[0].Path); !os.IsNotExist(err) {
		t.Fatal("selected directory still exists", err)
	}
	for _, f := range findings[1:] {
		if _, err := os.Stat(f.Path); err != nil {
			t.Fatal("unselected path changed", err)
		}
	}
	p.Expect(409, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, nil)
	// Replacing a report path with a symlink cannot redirect deletion.
	moved := findings[1].Path + "-moved"
	if err := os.Rename(findings[1].Path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(findings[2].Path, findings[1].Path); err != nil {
		t.Fatal(err)
	}
	p.Expect(202, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{entries[1].(object)["id"]}}, nil)
	waitCleanup(t, p, id)
	failed := p.Expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
	if failed["entries"].([]any)[1].(object)["status"] != "failed" {
		t.Fatal(failed)
	}
	if _, err := os.Stat(findings[2].Path); err != nil {
		t.Fatal("symlink target removed", err)
	}
	p.Expect(201, "POST", "/api/users", object{"username": "another", "password": "another-password-123", "role": "admin"}, nil)
	p.Login(false, "another", "another-password-123")
	p.Expect(404, "GET", "/api/agent/cleanups/"+id, nil, nil)
	p.Expect(404, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
	p.Expect(404, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{entries[2].(object)["id"]}}, nil)
	p.Expect(404, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
	p.Expect(201, "POST", "/api/users", object{"username": "viewer", "password": "viewer-password-123", "role": "viewer"}, nil)
	p.Login(false, "viewer", "viewer-password-123")
	p.Expect(403, "GET", "/api/agent/cleanup-reports", nil, nil)
	p.Expect(403, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
	p.Login(false, "administrator", "A-test-password-123")
	p.Expect(403, "DELETE", "/api/agent/cleanups/"+id, nil, map[string]string{"X-CSRF-Token": "wrong"})
	if result := p.Expect(200, "DELETE", "/api/agent/cleanups/"+id, nil, nil); result["ok"] != true {
		t.Fatal(result)
	}
	p.Expect(404, "GET", "/api/agent/cleanups/"+id, nil, nil)
	p.Expect(404, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
	var cleanupCount, entryCount, messageCount, reportCount, auditCount int
	for _, check := range []struct {
		query string
		count *int
	}{
		{"SELECT count(*) FROM agent_cleanups WHERE id=?", &cleanupCount},
		{"SELECT count(*) FROM agent_cleanup_entries WHERE cleanup_id=?", &entryCount},
		{"SELECT count(*) FROM agent_messages WHERE session_id=?", &messageCount},
		{"SELECT count(*) FROM agent_reports WHERE message_id=?", &reportCount},
		{"SELECT count(*) FROM audit WHERE action='storage.cleanup.remove' AND detail=?", &auditCount},
	} {
		arg := any(id)
		if check.count == &reportCount {
			arg = reportID
		}
		if err := p.db.SQL.QueryRow(check.query, arg).Scan(check.count); err != nil {
			t.Fatal(err)
		}
	}
	if cleanupCount != 0 || entryCount != 0 || messageCount != 0 || reportCount != 1 || auditCount != 1 {
		t.Fatalf("history deletion affected wrong records: cleanup=%d entries=%d messages=%d reports=%d audit=%d", cleanupCount, entryCount, messageCount, reportCount, auditCount)
	}
	reports = p.Expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
	if len(reports) != 1 || reports[0].(object)["cleanup_id"] != nil {
		t.Fatal("source report was not reusable", reports)
	}
	if _, err := os.Stat(findings[0].Path); !os.IsNotExist(err) {
		t.Fatal("deleting cleanup records changed filesystem results", err)
	}
	restarted := p.Expect(201, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
	newID := restarted["cleanup"].(object)["id"].(string)
	if newID == id {
		t.Fatal("history deletion reused removed cleanup ID")
	}
	if restarted["cleanup"].(object)["status"] != "ready" {
		t.Fatal(restarted)
	}
}

func waitCleanup(t *testing.T, p *testPlatform, id string) object {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result := p.Expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
		status := result["cleanup"].(object)["status"]
		if status != "running" && status != "cancelling" {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cleanup did not finish")
	return nil
}

func TestCleanupHistoryCannotBeDeletedWhileRunning(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	sourceID, cleanupID := platform.RandomHex(16), platform.RandomHex(16)
	for _, row := range []struct{ id, status string }{{sourceID, "completed"}} {
		if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model,report_scope) VALUES(?,?,'test',?,1,1,'completions','test','container')", row.id, userID, row.status); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.agent.saveReport(sourceID, "# report", nil, nil); err != nil {
		t.Fatal(err)
	}
	var reportID int64
	if err := p.db.SQL.QueryRow("SELECT message_id FROM agent_reports").Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanups(id,report_id,status,created_at,updated_at) VALUES(?,?,'running',1,1)", cleanupID, reportID); err != nil {
		t.Fatal(err)
	}
	p.Expect(409, "DELETE", "/api/agent/cleanups/"+cleanupID, nil, nil)
	if _, err := p.db.SQL.Exec("UPDATE agent_cleanups SET status='cancelled' WHERE id=?", cleanupID); err != nil {
		t.Fatal(err)
	}
	p.Expect(200, "DELETE", "/api/agent/cleanups/"+cleanupID, nil, nil)
}

func TestCleanupRestartKeepsUnconfirmedResults(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	sourceID, id := platform.RandomHex(16), platform.RandomHex(16)
	for _, sessionID := range []string{sourceID} {
		if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model,report_scope) VALUES(?,?,'test','running',1,1,'completions','test','container')", sessionID, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.agent.saveReport(sourceID, "# 空间消耗总报告", nil, nil); err != nil {
		t.Fatal(err)
	}
	var reportID int64
	p.db.SQL.QueryRow("SELECT message_id FROM agent_reports").Scan(&reportID)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanups(id,report_id,status,created_at,updated_at) VALUES(?,?,'running',1,1)", id, reportID); err != nil {
		t.Fatal(err)
	}
	entryID := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanup_entries(id,cleanup_id,path,category,summary,detail,status) VALUES(?,?,'/data/cache',1,'cache','{}','deleting')", entryID, id); err != nil {
		t.Fatal(err)
	}
	p.agent.Close()
	if err := Recover(p.db); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(p.agent.db, p.records, testAuthorization(p.db))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result, err := restarted.cleanup(id, userID)
	if err != nil {
		t.Fatal(err)
	}
	if result["cleanup"].(object)["status"] != "interrupted" || result["entries"].([]object)[0]["status"] != "uncertain" {
		t.Fatal(result)
	}
	// Reopening a cleanup record preserves unconfirmed deletion results.
	if _, err := restarted.prepareCleanup(reportID, userID, "administrator"); err != nil {
		t.Fatal(err)
	}
	result, err = restarted.cleanup(id, userID)
	if err != nil {
		t.Fatal(err)
	}
	if result["entries"].([]object)[0]["id"] != entryID || result["entries"].([]object)[0]["status"] != "uncertain" {
		t.Fatal(result)
	}
}

func TestCleanupCancelRetainsPartialResults(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	sourceID := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,snapshot_id,provider,model,report_scope) VALUES(?,?,'report','completed',1,1,?,'responses','test','container')", sourceID, userID, platform.RandomHex(16)); err != nil {
		t.Fatal(err)
	}
	findings := []reportFinding{{Path: "/data/cache-a", Category: 1, Summary: "cache"}, {Path: "/data/cache-b", Category: 1, Summary: "cache"}}
	if err := p.agent.saveReport(sourceID, "# report", []reportSubject{{ID: "container"}}, []reportResult{{SubjectID: "container", Findings: findings}}); err != nil {
		t.Fatal(err)
	}
	reports := p.Expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
	prepared := p.Expect(201, "POST", "/api/agent/cleanups", object{"report_id": reports[0].(object)["report_id"]}, nil)
	id := prepared["cleanup"].(object)["id"].(string)
	entries := prepared["entries"].([]any)
	ids := []string{entries[0].(object)["id"].(string), entries[1].(object)["id"].(string)}
	started := make(chan struct{})
	p.records.cleanup = func(ctx context.Context, actor, snapshotID string, paths []string, password []byte, result func(string, string, string) error) error {
		if err := result(paths[0], "deleted", ""); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	p.Expect(202, "POST", "/api/agent/cleanups/"+id+"/delete", object{"entry_ids": ids, "sudo_password": testDeletePassword}, nil)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start")
	}
	p.Expect(409, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
	p.Expect(200, "POST", "/api/agent/cleanups/"+id+"/cancel", object{}, nil)
	state := waitCleanup(t, p, id)
	if state["cleanup"].(object)["status"] != "cancelled" || state["entries"].([]any)[0].(object)["status"] != "deleted" || state["entries"].([]any)[1].(object)["status"] != "uncertain" {
		t.Fatal(state)
	}
}
