package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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

func TestCleanupReadsFullReportValidatesCoverageAndDeletesSelectedPaths(t *testing.T) {
	for _, protocol := range []string{"completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
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
			job := p.expect(202, "POST", "/api/jobs", object{}, nil)
			snapshotID := job["id"].(string)
			waitJob(t, p.records, snapshotID)
			var userID string
			p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID)
			sourceID := platform.RandomHex(16)
			_, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,snapshot_id,provider,model) VALUES(?,?,'空间消耗总报告','completed',1,1,?,?, 'test')", sourceID, userID, snapshotID, protocol)
			if err != nil {
				t.Fatal(err)
			}
			containers := []reportContainer{{ID: "container", Name: "name"}}
			results := []reportContainerResult{{ContainerID: "container", Findings: findings}}
			fullReport := "# 空间消耗总报告\n" + strings.Repeat("完整报告正文，不应截断。", 4000) + "\nREPORT_END_MARKER\n" + renderReportFindings(containers, results)
			if err = p.agent.saveReport(sourceID, fullReport, containers, results); err != nil {
				t.Fatal(err)
			}
			// Followups must never replace the full report used for extraction.
			p.agent.message(sourceID, "assistant", "后续追问只提到一个目录", "")
			reports := p.expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
			reportID := reports[0].(object)["report_id"]
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				json.NewDecoder(r.Body).Decode(&body)
				if body["tools"] != nil {
					t.Error("extraction must not expose tools")
				}
				if !strings.Contains(httpapi.JSONText(body), "REPORT_END_MARKER") {
					t.Error("full report was truncated")
				}
				rows := []object{}
				for _, f := range findings {
					rows = append(rows, object{"path": f.Path, "category": f.Category, "summary": "提取用途与处理条件"})
				}
				if calls.Add(1) == 1 {
					rows = rows[:1]
				}
				content := httpapi.JSONText(object{"entries": rows})
				if protocol == "completions" {
					writeModelReply(w, object{"choices": []object{{"message": object{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
				} else {
					writeModelReply(w, object{"status": "completed", "output": []object{{"type": "message", "role": "assistant", "content": []object{{"type": "output_text", "text": content}}}}})
				}
			}))
			defer mock.Close()
			configureTestAgent(t, p, protocol, mock.URL)
			p.expect(400, "POST", "/api/agent/cleanups", object{"report_id": reportID, "report": "override"}, nil)
			started := p.expect(202, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
			id := started["id"].(string)
			completed := waitAgentSession(t, p, id)
			if completed["session"].(object)["status"] != "completed" {
				t.Fatal(completed)
			}
			if calls.Load() != 2 {
				t.Fatalf("expected coverage repair, got %d calls", calls.Load())
			}
			var requests, responses, validations int
			if err := p.db.SQL.QueryRow("SELECT count(*) FILTER (WHERE role='model_request'), count(*) FILTER (WHERE role='model_response'), count(*) FILTER (WHERE role='cleanup_validation') FROM agent_messages WHERE session_id=?", id).Scan(&requests, &responses, &validations); err != nil || requests != 2 || responses != 2 || validations != 1 {
				t.Fatalf("extraction trace was not persisted: requests=%d responses=%d validations=%d err=%v", requests, responses, validations, err)
			}
			state := p.expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
			entries := state["entries"].([]any)
			if len(entries) != 4 {
				t.Fatal(state)
			}
			first := entries[0].(object)
			deleteURL := "/api/agent/cleanups/" + id + "/delete"
			// Reopening the same extraction must not call the model or discard outcomes.
			again := p.expect(202, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
			if again["id"] != id || calls.Load() != 2 {
				t.Fatal(again)
			}
			p.expect(409, "POST", "/api/agent/sessions/"+id+"/messages", object{"message": "followup"}, nil)
			p.expect(400, "POST", deleteURL, object{"paths": []string{p.storage}}, nil)
			p.expect(400, "POST", deleteURL, object{"entry_ids": []any{first["id"]}}, nil)
			for _, password := range []any{"", nil, 123, "line\nbreak", strings.Repeat("x", 1025)} {
				p.expect(400, "POST", deleteURL, object{"entry_ids": []any{first["id"]}, "sudo_password": password}, nil)
			}

			p.expect(409, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []string{platform.RandomHex(16)}}, nil)
			p.expect(400, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []string{first["id"].(string), first["id"].(string)}}, nil)
			p.expect(403, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, map[string]string{"X-CSRF-Token": "wrong"})
			p.expect(202, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, nil)
			waitAgentSession(t, p, id)
			deleted := p.expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
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
			p.expect(409, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{first["id"]}}, nil)
			// Replacing a report path with a symlink cannot redirect deletion.
			moved := findings[1].Path + "-moved"
			if err := os.Rename(findings[1].Path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(findings[2].Path, findings[1].Path); err != nil {
				t.Fatal(err)
			}
			p.expect(202, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{entries[1].(object)["id"]}}, nil)
			waitAgentSession(t, p, id)
			failed := p.expect(200, "GET", "/api/agent/cleanups/"+id, nil, nil)
			if failed["entries"].([]any)[1].(object)["status"] != "failed" {
				t.Fatal(failed)
			}
			if _, err := os.Stat(findings[2].Path); err != nil {
				t.Fatal("symlink target removed", err)
			}
			p.expect(201, "POST", "/api/users", object{"username": "another", "password": "another-password-123", "role": "admin"}, nil)
			p.login(false, "another", "another-password-123")
			p.expect(404, "GET", "/api/agent/cleanups/"+id, nil, nil)
			p.expect(404, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
			p.expect(404, "POST", deleteURL, object{"sudo_password": testDeletePassword, "entry_ids": []any{entries[2].(object)["id"]}}, nil)
			p.expect(404, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
			p.expect(201, "POST", "/api/users", object{"username": "viewer", "password": "viewer-password-123", "role": "viewer"}, nil)
			p.login(false, "viewer", "viewer-password-123")
			p.expect(403, "GET", "/api/agent/cleanup-reports", nil, nil)
			p.expect(403, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
			_, _, asset := p.request("GET", "/cleanup.js", nil, nil)
			if asset.Code != 200 || !strings.Contains(asset.Body.String(), "cleanupSudoPassword") {
				t.Fatal("cleanup frontend asset not served")
			}
			p.login(false, "administrator", "A-test-password-123")
			p.expect(403, "DELETE", "/api/agent/cleanups/"+id, nil, map[string]string{"X-CSRF-Token": "wrong"})
			if result := p.expect(200, "DELETE", "/api/agent/cleanups/"+id, nil, nil); result["ok"] != true {
				t.Fatal(result)
			}
			p.expect(404, "GET", "/api/agent/cleanups/"+id, nil, nil)
			p.expect(404, "DELETE", "/api/agent/cleanups/"+id, nil, nil)
			var cleanupCount, entryCount, messageCount, reportCount, auditCount int
			for _, check := range []struct {
				query string
				count *int
			}{
				{"SELECT count(*) FROM agent_cleanups WHERE session_id=?", &cleanupCount},
				{"SELECT count(*) FROM agent_cleanup_entries WHERE session_id=?", &entryCount},
				{"SELECT count(*) FROM agent_messages WHERE session_id=?", &messageCount},
				{"SELECT count(*) FROM agent_reports WHERE message_id=?", &reportCount},
				{"SELECT count(*) FROM audit WHERE action='agent.extract.delete' AND detail=?", &auditCount},
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
			reports = p.expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
			if len(reports) != 1 || reports[0].(object)["cleanup_id"] != nil {
				t.Fatal("source report was not reusable", reports)
			}
			if _, err := os.Stat(findings[0].Path); !os.IsNotExist(err) {
				t.Fatal("deleting extraction history changed filesystem cleanup results", err)
			}
			restarted := p.expect(202, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
			newID := restarted["id"].(string)
			if newID == id {
				t.Fatal("history deletion reused removed session")
			}
			if result := waitAgentSession(t, p, newID); result["session"].(object)["status"] != "completed" {
				t.Fatal("report could not be extracted again", result)
			}

		})
	}
}

func TestCleanupHistoryCannotBeDeletedWhileRunning(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	sourceID, cleanupID := platform.RandomHex(16), platform.RandomHex(16)
	for _, row := range []struct{ id, status string }{{sourceID, "completed"}, {cleanupID, "running"}} {
		if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model) VALUES(?,?,'test',?,1,1,'completions','test')", row.id, userID, row.status); err != nil {
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
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanups(session_id,report_id) VALUES(?,?)", cleanupID, reportID); err != nil {
		t.Fatal(err)
	}
	p.expect(409, "DELETE", "/api/agent/cleanups/"+cleanupID, nil, nil)
	if _, err := p.db.SQL.Exec("UPDATE agent_sessions SET status='cancelled' WHERE id=?", cleanupID); err != nil {
		t.Fatal(err)
	}
	p.expect(200, "DELETE", "/api/agent/cleanups/"+cleanupID, nil, nil)
}

func TestCleanupParserRejectsInventedAndReclassifiedPaths(t *testing.T) {
	manifest := []*mergedReportFinding{{reportFinding: reportFinding{Path: "/data/a", Category: 3}}}
	for _, raw := range []string{`{"entries":null}`, `{"entries":[]}`, `{"entries":[{"path":"/data/b","category":3,"summary":"x"}]}`, `{"entries":[{"path":"/data/a","category":1,"summary":"x"}]}`, `{"entries":[{"path":"/data/a","category":3,"summary":""}]}`, `{"entries":[{"path":"/data/a","category":3,"summary":"x","delete":true}]}`} {
		if _, err := parseCleanup(raw, manifest); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parseCleanup(`{"entries":[]}`, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRestartKeepsUnconfirmedResults(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	sourceID, id := platform.RandomHex(16), platform.RandomHex(16)
	for _, sessionID := range []string{sourceID, id} {
		if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model) VALUES(?,?,'test','running',1,1,'completions','test')", sessionID, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.agent.saveReport(sourceID, "# 空间消耗总报告", nil, nil); err != nil {
		t.Fatal(err)
	}
	var reportID int64
	p.db.SQL.QueryRow("SELECT message_id FROM agent_reports").Scan(&reportID)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanups(session_id,report_id,phase) VALUES(?,?,'delete')", id, reportID); err != nil {
		t.Fatal(err)
	}
	entryID := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_cleanup_entries(id,session_id,path,category,summary,detail,status) VALUES(?,?,'/data/cache',1,'cache','{}','deleting')", entryID, id); err != nil {
		t.Fatal(err)
	}
	p.agent.Close()
	restarted, err := NewManager(p.agent.db, p.records)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result, err := restarted.cleanup(id, userID)
	if err != nil {
		t.Fatal(err)
	}
	if result["session"].(object)["status"] != "interrupted" || result["entries"].([]object)[0]["status"] != "uncertain" {
		t.Fatal(result)
	}
	// Reopening extraction cannot erase deletion history or restart a deletion.
	if _, err := restarted.startCleanup(reportID, userID, "administrator"); err != nil {
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
