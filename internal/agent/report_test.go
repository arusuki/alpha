package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
		body["concurrency"] = 1
		p.expect(400, "POST", "/api/agent/reports", body, nil)
	}
	for _, limit := range []any{nil, 0, -1, 17, 1.5, "2"} {
		p.expect(400, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": limit}, nil)
	}
	p.expect(400, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0}, nil)
	p.expect(403, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": 1}, map[string]string{"X-CSRF-Token": "wrong"})
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 9, "concurrency": 1}, nil)
	p.expect(404, "POST", "/api/agent/reports", object{"snapshot_id": strings.Repeat("f", 32), "revision": 0, "concurrency": 1}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='failed' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='completed',trigger='incremental' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	p.expect(409, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	var count int
	p.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions").Scan(&count)
	if count != 0 || called.Load() {
		t.Fatal("invalid report created a session or reached model")
	}
	p.expect(201, "POST", "/api/users", object{"username": "viewer", "password": "A-viewer-password-123", "role": "viewer"}, nil)
	p.login(false, "viewer", "A-viewer-password-123")
	p.expect(403, "POST", "/api/agent/reports", object{"snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
}
