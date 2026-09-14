package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestDiskReportUsesSelectedRecordAndSupportsFollowup(t *testing.T) {
	for _, protocol := range []string{"completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			selected := p.expect(202, "POST", "/api/jobs", object{}, nil)
			id := selected["id"].(string)
			waitJob(t, p.records, id)
			// A newer record must not replace the one selected in the UI.
			latest := p.expect(202, "POST", "/api/jobs", object{}, nil)
			waitJob(t, p.records, latest["id"].(string))
			p.records.failStart = true
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded := httpapi.JSONText(body)
				if n := calls.Add(1); n == 1 {
					for _, needle := range []string{id, "生成空间消耗总报告", "可写层排行", "roots", "bind mount", "90–180", "Hugging Face", "待确认"} {
						if !strings.Contains(encoded, needle) {
							t.Errorf("report context missing %q", needle)
						}
					}
					if strings.Contains(encoded, latest["id"].(string)) {
						t.Error("report used latest record instead of selected record")
					}
				} else if !strings.Contains(encoded, "继续查看缓存") {
					t.Error("followup missing")
				}
				answer := "# 空间消耗总报告\n\n## 总览与主要结论\n已核对所选记录。"
				if protocol == "completions" {
					httpapi.WriteJSON(w, 200, object{"choices": []object{{"message": object{"role": "assistant", "content": answer}}}})
				} else {
					httpapi.WriteJSON(w, 200, object{"status": "completed", "output": []object{{"type": "message", "role": "assistant", "content": []object{{"type": "output_text", "text": answer}}}}})
				}
			}))
			defer mock.Close()
			configureTestAgent(t, p, protocol, mock.URL)
			created := p.expect(202, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, nil)
			sessionID := created["id"].(string)
			result := waitAgentSession(t, p, sessionID)
			if result["session"].(object)["status"] != "completed" || created["snapshot_id"] != id || created["title"] != "空间消耗总报告" {
				t.Fatalf("bad report: %v", result)
			}
			p.expect(202, "POST", "/api/agent/sessions/"+sessionID+"/messages", object{"message": "继续查看缓存"}, nil)
			result = waitAgentSession(t, p, sessionID)
			if result["session"].(object)["status"] != "completed" || calls.Load() != 2 {
				t.Fatalf("followup failed: %v", result)
			}
			var count int
			p.db.SQL.QueryRow("SELECT count(*) FROM jobs").Scan(&count)
			if count != 2 {
				t.Fatalf("report or followup unexpectedly scanned: %d jobs", count)
			}
			p.expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
			p.expect(409, "POST", "/api/agent/sessions/"+sessionID+"/messages", object{"message": "继续"}, nil)
			if calls.Load() != 2 {
				t.Fatal("deleted record silently analyzed another record")
			}
		})
	}
}

func TestDiskReportValidatesSourceBeforeCallingModel(t *testing.T) {
	p := newTestPlatform(t)
	p.expect(401, "POST", "/api/agent/reports", object{}, nil)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	id := job["id"].(string)
	waitJob(t, p.records, id)
	var called atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	for _, body := range []object{
		{}, {"snapshot_id": id}, {"snapshot_id": id, "revision": nil},
		{"snapshot_id": id, "revision": -1}, {"snapshot_id": id, "revision": "0"},
		{"snapshot_id": id, "revision": 0.5}, {"snapshot_id": "../bad", "revision": 0},
		{"snapshot_id": id, "revision": 0, "message": "override report"},
	} {
		p.expect(400, "POST", "/api/agent/reports", body, nil)
	}
	p.expect(403, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, map[string]string{"X-CSRF-Token": "wrong"})
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 9}, nil)
	p.expect(404, "POST", "/api/agent/reports", object{"snapshot_id": strings.Repeat("f", 32), "revision": 0}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='failed' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='completed',trigger='incremental' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, nil)
	var count int
	p.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions").Scan(&count)
	if count != 0 || called.Load() {
		t.Fatal("invalid report created a session or reached model")
	}
	p.expect(201, "POST", "/api/users", object{"username": "viewer", "password": "A-viewer-password-123", "role": "viewer"}, nil)
	p.login(false, "viewer", "A-viewer-password-123")
	p.expect(403, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, nil)
}
