package storage

import (
	"context"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

func TestScheduledAnalysisQueueAndRetention(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	c := p.configure()
	c.AutoAgentAnalyze, c.ScheduleMode, c.IntervalMinutes = true, "interval", 5
	p.Expect(200, "PUT", "/api/settings", object{"revision": 2, "value": c}, nil)
	if err := p.m.tick(); err != nil {
		t.Fatal(err)
	}
	var id, owner, expectedOwner string
	if err := p.db.SQL.QueryRow("SELECT id,analysis_user_id FROM jobs WHERE trigger='scheduled'").Scan(&id, &owner); err != nil {
		t.Fatal(err)
	}
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&expectedOwner); err != nil || owner != expectedOwner {
		t.Fatalf("owner=%q expected=%q: %v", owner, expectedOwner, err)
	}
	if job := waitJob(t, p.db, id); job["status"] != "completed" {
		t.Fatal(job)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.api.Wait(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	queue := p.Expect(200, "GET", "/api/worker/scheduled-analysis", nil, nil)["jobs"].([]any)
	if len(queue) != 1 || queue[0].(object)["id"] != id {
		t.Fatal(queue)
	}
	manual := p.Expect(202, "POST", "/api/jobs", object{}, nil)
	if err := p.api.Wait(ctx, manual["id"].(string), nil); err != nil {
		t.Fatal(err)
	}
	p.Expect(409, "DELETE", "/api/jobs/"+id, nil, nil)
	p.m.mu.Lock()
	err := p.m.reapLocked()
	if err == nil {
		err = p.m.pruneLocked(1)
	}
	p.m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.job(id); err != nil {
		t.Fatal("pending analysis lost its scan", err)
	}
	// Failed scans, manual scans and incremental work cannot enter the queue.
	for _, trigger := range []string{"manual", "incremental", "scheduled"} {
		status := "completed"
		if trigger == "scheduled" {
			status = "failed"
		}
		if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config,analysis_status) VALUES(?,?,?,'test',0,'{}','pending')", platform.RandomHex(16), status, trigger); err != nil {
			t.Fatal(err)
		}
	}
	if queue := p.Expect(200, "GET", "/api/worker/scheduled-analysis", nil, nil)["jobs"].([]any); len(queue) != 1 {
		t.Fatal(queue)
	}
	for range 2 {
		p.Expect(200, "POST", "/api/worker/scheduled-analysis", object{"id": id, "status": "completed", "error": ""}, nil)
	}
	if queue := p.Expect(200, "GET", "/api/worker/scheduled-analysis", nil, nil)["jobs"].([]any); len(queue) != 0 {
		t.Fatal(queue)
	}
	p.Expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
}
