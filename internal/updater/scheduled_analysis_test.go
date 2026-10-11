package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"project-alpha/internal/platform"
)

func scheduledAnalysisOldDatabase(t *testing.T, role string) *platform.Database {
	t.Helper()
	db := oldDatabase(t, role)
	if role == "worker" {
		if _, err := db.SQL.Exec(`INSERT INTO settings(id,value) VALUES(1,'{"root":[],"exclude":[],"no_docker":false,"include_docker_root":false,"max_depth":5,"max_nodes":50000,"owner_label":"project-alpha.owner","docker_timeout":120,"interval_minutes":0,"scan_backend":"auto","scan_mode":"normal"}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`UPDATE settings SET value=json_set(value,'$.schedule_mode','interval','$.schedule_times',json('[]'),
'$.schedule_weekdays',json('[0,1,2,3,4,5,6]'),'$.schedule_timezone','UTC','$.retain_records',7);`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`UPDATE settings SET value=json_set(value,'$.root',json('["/kept"]'),'$.interval_minutes',60),revision=12;
INSERT INTO jobs(id,status,trigger,created_by,created_at,config) SELECT 'kept-scan','completed','scheduled','scheduler',123,value FROM settings;`); err != nil {
			t.Fatal(err)
		}
	}
	if role == "control" {
		if _, err := db.SQL.Exec(`INSERT INTO agent_sessions(id,user_id,node_id,title,status,created_at,updated_at,snapshot_id,provider,model,report_scope)
VALUES('kept-session','user','node','kept report','completed',123,124,'kept-scan','completions','test','host');
INSERT INTO agent_messages(id,session_id,role,content,created_at) VALUES(1,'kept-session','assistant','existing report',124);
INSERT INTO agent_reports VALUES(1,'[]');`); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func runScheduledMigration(t *testing.T, binary, entry, role string, db *platform.Database) error {
	t.Helper()
	var cmd *exec.Cmd
	if entry == "--database-only" {
		cmd = exec.Command(binary, entry, "--data-dir", db.Directory)
	} else {
		lock, err := db.LockService()
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		cmd = exec.Command(binary, "_migrate", role, db.Directory)
		cmd.ExtraFiles = []*os.File{lock}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}

func TestScheduledAnalysisMigrationRollback(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"worker", "control"} {
			t.Run(entry+"/rollback/"+role, func(t *testing.T) {
				db := scheduledAnalysisOldDatabase(t, role)
				var query, check string
				if role == "worker" {
					query = "CREATE TRIGGER reject_upgrade BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT,'injected migration failure'); END"
					check = "SELECT count(*) FROM pragma_table_info('jobs') WHERE name='analysis_status'"
				} else {
					query = "CREATE INDEX idx_agent_scheduled ON agent_sessions(title)"
					check = "SELECT count(*) FROM pragma_table_info('agent_sessions') WHERE name='scheduled_job_id'"
				}
				if _, err := db.SQL.Exec(query); err != nil {
					t.Fatal(err)
				}
				if err := runScheduledMigration(t, binary, entry, role, db); err == nil {
					t.Fatal("failed migration succeeded")
				}
				assertVersion(t, db, 38)
				var columns int
				if err := db.SQL.QueryRow(check).Scan(&columns); err != nil || columns != 0 {
					t.Fatalf("partial migration not rolled back: %d %v", columns, err)
				}
			})
		}
	}
}
