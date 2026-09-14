package agent

import (
	"testing"

	"project-alpha/internal/platform"
)

func TestDeleteScanAgentReferences(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "admin", "administrator-password")
	p.configure()
	job, err := p.records.StartOverview("admin")
	if err != nil {
		t.Fatal(err)
	}
	id := job["id"].(string)
	waitJob(t, p.records, id)
	var uid string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='admin'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	session := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,snapshot_id,active_job_id,provider,model) VALUES(?,?,?,'running',0,0,?,?,'test','test')", session, uid, "analysis", id, id); err != nil {
		t.Fatal(err)
	}

	p.expect(409, "DELETE", "/api/jobs/"+id, nil, nil)
	if _, err := p.db.SQL.Exec("UPDATE agent_sessions SET status='completed' WHERE id=?", session); err != nil {
		t.Fatal(err)
	}
	p.expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
	var count int

	if err := p.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions WHERE id=? AND snapshot_id IS NULL AND active_job_id IS NULL", session).Scan(&count); err != nil || count != 1 {
		t.Fatalf("session references remain %d %v", count, err)
	}
}
