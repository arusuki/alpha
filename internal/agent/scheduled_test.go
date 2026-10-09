package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func TestScheduledReportsExtractEntriesAndResumeWithoutDuplicates(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	p.configure()
	job, err := p.m.Start("scheduler", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := job["id"].(string)
	waitJob(t, p.records, snapshotID)
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(p.storage, "model.bin")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeReportReply(w, "completions", "查询目录", []agentToolCall{{ID: "host", Name: "get_host_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "offset": 0, "limit": 30})}})
			return
		}
		writeReportReply(w, "completions", httpapi.JSONText(object{"directories": []object{{"path": p.storage, "findings": []object{{"path": file, "category": 1, "kind": "下载与包缓存", "bytes": info.Size(), "summary": "测试缓存", "reason": "可重新生成"}}, "note": ""}}}), nil)
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	if done, err := p.agent.AdvanceScheduled(snapshotID, userID, "administrator"); done || err != nil {
		t.Fatalf("start: done=%v err=%v", done, err)
	}
	var sessionID string
	if err := p.db.SQL.QueryRow("SELECT id FROM agent_sessions WHERE scheduled_job_id=?", snapshotID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	finished := waitAgentSession(t, p, sessionID)
	if finished["session"].(object)["status"] != "completed" {
		t.Fatal(finished)
	}
	// Restart after report publication but before extraction/acknowledgement.
	p.agent.Close()
	manager, err := NewManager(NewStore(p.db), p.records, testAuthorization(p.db))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		done, err := manager.AdvanceScheduled(snapshotID, userID, "administrator")
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic analysis timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for range 2 {
		if done, err := manager.AdvanceScheduled(snapshotID, userID, "administrator"); !done || err != nil {
			t.Fatalf("repeat: done=%v err=%v", done, err)
		}
	}
	for query, expected := range map[string]int{
		"SELECT count(*) FROM agent_sessions":                                                       2,
		"SELECT count(*) FROM agent_reports":                                                        2,
		"SELECT count(*) FROM agent_cleanups WHERE status='ready'":                                  2,
		"SELECT count(*) FROM agent_cleanup_entries WHERE path='" + file + "' AND status='pending'": 1,
	} {
		var count int
		if err := p.db.SQL.QueryRow(query).Scan(&count); err != nil || count != expected {
			t.Fatalf("%s = %d, expected %d: %v", query, count, expected, err)
		}
	}
	if _, err := os.Stat(file); err != nil || calls.Load() != 2 {
		t.Fatalf("source file changed or report repeated: calls=%d err=%v", calls.Load(), err)
	}
}

func TestScheduledReportStopDoesNotLaunchAnotherScope(t *testing.T) {
	for _, status := range []string{"cancelled", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			p := newTestPlatform(t)
			p.Login(true, "administrator", "A-test-password-123")
			var userID string
			if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
				t.Fatal(err)
			}
			if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model,report_scope,scheduled_job_id) VALUES('stopped',?,'scheduled',?,1,2,'completions','test','host','scan')", userID, status); err != nil {
				t.Fatal(err)
			}
			if done, err := p.agent.AdvanceScheduled("scan", userID, "administrator"); !done || err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("stop result: %v %v", done, err)
			}
			var count int
			if err := p.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions").Scan(&count); err != nil || count != 1 {
				t.Fatalf("unexpected next report: %d %v", count, err)
			}
		})
	}
}
