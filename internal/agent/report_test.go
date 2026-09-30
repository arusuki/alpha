package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiskReportValidatesSourceBeforeCallingModel(t *testing.T) {
	p := newTestPlatform(t)
	p.Expect(401, "POST", "/api/agent/reports", object{}, nil)
	p.Login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.Expect(202, "POST", "/api/jobs", object{}, nil)
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
		body["concurrency"] = 1
		body["scope"] = "container"
		p.Expect(400, "POST", "/api/agent/reports", body, nil)
	}
	p.Expect(400, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	for _, scope := range []any{"", "both", nil, 1} {
		p.Expect(400, "POST", "/api/agent/reports", object{"scope": scope, "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	}
	for _, limit := range []any{nil, 0, -1, 17, 1.5, "2"} {
		p.Expect(400, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": limit}, nil)
	}
	p.Expect(400, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0}, nil)
	p.Expect(403, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, map[string]string{"X-CSRF-Token": "wrong"})
	p.Expect(409, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 9, "concurrency": 1}, nil)
	p.Expect(404, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": strings.Repeat("f", 32), "revision": 0, "concurrency": 1}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='failed' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.Expect(409, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='completed',trigger='incremental' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.Expect(409, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	var count int
	p.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions").Scan(&count)
	if count != 0 || called.Load() {
		t.Fatal("invalid report created a session or reached model")
	}
	p.Expect(201, "POST", "/api/users", object{"username": "viewer", "password": "A-viewer-password-123", "role": "viewer"}, nil)
	p.Login(false, "viewer", "A-viewer-password-123")
	p.Expect(403, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
}

func TestSessionHistoryLimitsEachReportScopeSeparately(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	tx, err := p.db.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	wantIDs := map[string]bool{}
	for scopeNumber, scope := range []string{"", "host", "container"} {
		for i := 0; i < 52; i++ {
			id := fmt.Sprintf("%032x", scopeNumber*1000+i)
			if i >= 2 {
				wantIDs[id] = true
			}
			updated := float64(scopeNumber*1000 + i)
			if _, err := tx.Exec(`INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model,report_scope)
 VALUES(?,?,?,'completed',?,?,'completions','test',?)`, id, userID, "history", updated, updated, scope); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows := p.Expect(200, "GET", "/api/agent/sessions", nil, nil)["sessions"].([]any)
	if len(rows) != 150 {
		t.Fatalf("history returned %d sessions, want 50 per scope", len(rows))
	}
	counts := map[string]int{}
	for _, value := range rows {
		row := value.(map[string]any)
		scope := row["report_scope"].(string)
		counts[scope]++
		id := row["id"].(string)
		if !wantIDs[id] {
			t.Fatalf("unexpected or out-of-range session %s in %q history", id, scope)
		}
		delete(wantIDs, id)
	}
	if len(wantIDs) != 0 {
		t.Fatalf("history omitted %d expected sessions", len(wantIDs))
	}
	for _, scope := range []string{"", "host", "container"} {
		if counts[scope] != 50 {
			t.Fatalf("%q history returned %d sessions, want 50", scope, counts[scope])
		}
	}
}
