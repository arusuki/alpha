package storage

import (
	"os"
	"path/filepath"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func deleteFixture(t *testing.T, p *testPlatform, status, trigger, base string) string {
	t.Helper()
	id := platform.RandomHex(16)
	_, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,?,?,?,?,?,?)", id, status, trigger, "admin", platform.Now(), platform.Now(), httpapi.JSONText(object{"base_job_id": base}))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.db.Directory, "results", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "snapshot.json"), []byte(`{"saved":true}`))
	return id
}

func TestDeleteScanRecords(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "admin", "administrator-password")
	older := deleteFixture(t, p, "completed", "manual", "")
	base := deleteFixture(t, p, "completed", "manual", "")
	child := deleteFixture(t, p, "completed", "incremental", base)
	failed := deleteFixture(t, p, "failed", "incremental", base)
	if _, err := p.db.SQL.Exec("INSERT INTO snapshot_records(job_id,revision,root_path,metadata) VALUES(?,1,'@root','{}')", base); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.SQL.Exec("INSERT INTO snapshot_changes(job_id,revision,path) VALUES(?,1,'/data')", base); err != nil {
		t.Fatal(err)
	}
	p.expect(200, "DELETE", "/api/jobs/"+child, nil, nil)
	var revision int
	if err := p.db.SQL.QueryRow("SELECT revision FROM snapshot_records WHERE job_id=?", base).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("deleting directory worker removed published data", err)
	}
	result := p.expect(200, "DELETE", "/api/jobs/"+base, nil, nil)
	if len(result["deleted_ids"].([]any)) != 2 || result["cleanup_pending"] != false {
		t.Fatal(result)
	}
	for _, id := range []string{base, child, failed} {
		p.expect(404, "GET", "/api/jobs/"+id, nil, nil)
		p.expect(404, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
		if _, err := os.Stat(filepath.Join(p.db.Directory, "results", id)); !os.IsNotExist(err) {
			t.Fatalf("result remains: %s %v", id, err)
		}
	}
	p.expect(404, "DELETE", "/api/jobs/"+base, nil, nil)
	if got := p.expect(200, "GET", "/api/state", nil, nil)["latest_id"]; got != older {
		t.Fatal(got)
	}
	for _, table := range []string{"snapshot_records", "snapshot_changes"} {
		var n int
		if err := p.db.SQL.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", table, n, err)
		}
	}
	var detail string
	if err := p.db.SQL.QueryRow("SELECT detail FROM audit WHERE action='scan.delete' AND actor='admin'").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	p.expect(200, "DELETE", "/api/jobs/"+older, nil, nil)
	if got := p.expect(200, "GET", "/api/state", nil, nil)["latest_id"]; got != nil {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(p.storage, "model.bin")); err != nil {
		t.Fatal("source file removed", err)
	}
}

func TestDeleteScanProtection(t *testing.T) {
	p := newTestPlatform(t)
	id := deleteFixture(t, p, "completed", "manual", "")
	p.expect(401, "DELETE", "/api/jobs/"+id, nil, nil)
	p.login(true, "admin", "administrator-password")
	p.expect(403, "DELETE", "/api/jobs/"+id, nil, map[string]string{"X-CSRF-Token": "bad"})
	p.expect(403, "DELETE", "/api/jobs/"+id, nil, map[string]string{"Origin": "http://evil.example"})
	p.expect(201, "POST", "/api/users", object{"username": "viewer", "password": "viewer-password-long", "role": "viewer"}, nil)
	p.login(false, "viewer", "viewer-password-long")
	p.expect(403, "DELETE", "/api/jobs/"+id, nil, nil)
	p.login(false, "admin", "administrator-password")
	for _, status := range []string{"queued", "running", "cancelling"} {
		child := deleteFixture(t, p, status, "incremental", id)
		p.expect(409, "DELETE", "/api/jobs/"+child, nil, nil)
		p.expect(409, "DELETE", "/api/jobs/"+id, nil, nil)
		if _, err := p.db.SQL.Exec("UPDATE jobs SET status='cancelled' WHERE id=?", child); err != nil {
			t.Fatal(err)
		}
	}
	p.expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
	for _, status := range []string{"failed", "cancelled", "interrupted"} {
		record := deleteFixture(t, p, status, "manual", "")
		p.expect(200, "DELETE", "/api/jobs/"+record, nil, nil)
	}
}

func TestDeleteScanRollback(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "admin", "administrator-password")
	id := deleteFixture(t, p, "completed", "manual", "")
	if _, err := p.db.SQL.Exec("CREATE TRIGGER reject_scan_delete BEFORE INSERT ON audit WHEN NEW.action='scan.delete' BEGIN SELECT RAISE(ABORT,'test rollback'); END"); err != nil {
		t.Fatal(err)
	}
	p.expect(500, "DELETE", "/api/jobs/"+id, nil, nil)
	p.expect(200, "GET", "/api/jobs/"+id, nil, nil)
	if _, err := os.Stat(filepath.Join(p.db.Directory, "results", id, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
}
