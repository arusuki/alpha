package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func scheduledAnalysisOldDatabase(t *testing.T, role string, version int) *platform.Database {
	t.Helper()
	db := oldDatabase(t, role)
	if role == "worker" {
		if _, err := db.SQL.Exec(`INSERT INTO settings(id,value) VALUES(1,'{"root":[],"exclude":[],"no_docker":false,"include_docker_root":false,"max_depth":5,"max_nodes":50000,"owner_label":"project-alpha.owner","docker_timeout":120,"interval_minutes":0,"scan_backend":"auto","scan_mode":"normal"}')`); err != nil {
			t.Fatal(err)
		}
	}
	if role == "worker" && version >= 37 {
		if _, err := db.SQL.Exec(`ALTER TABLE settings ADD COLUMN schedule_last_run REAL NOT NULL DEFAULT 0;
UPDATE settings SET value=json_set(value,'$.schedule_mode','interval','$.schedule_times',json('[]'),
'$.schedule_weekdays',json('[0,1,2,3,4,5,6]'),'$.schedule_timezone','UTC','$.retain_records',7);`); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 38 {
		schema, err := os.ReadFile("testdata/v0.6.0/mihomo.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.SQL.Exec(string(schema)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
	if role == "worker" {
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

func TestScheduledAnalysisBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"worker", "control", "registry"} {
			for _, version := range []int{36, 37, 38} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := scheduledAnalysisOldDatabase(t, role, version)
					for range 2 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
						assertVersion(t, db, platform.DatabaseVersion)
					}
					if role == "worker" {
						var root, owner, state string
						var enabled, interval, revision, historyEnabled int
						if err := db.SQL.QueryRow("SELECT json_extract(value,'$.root[0]'),json_extract(value,'$.auto_agent_analyze'),json_extract(value,'$.interval_minutes'),revision,analysis_user_id FROM settings").Scan(&root, &enabled, &interval, &revision, &owner); err != nil || root != "/kept" || enabled != 0 || interval != 60 || owner != "" || revision < 13 {
							t.Fatalf("settings changed: %s %d %d %d %s: %v", root, enabled, interval, revision, owner, err)
						}
						if err := db.SQL.QueryRow("SELECT analysis_status,json_extract(config,'$.auto_agent_analyze') FROM jobs WHERE id='kept-scan'").Scan(&state, &historyEnabled); err != nil || state != "" || historyEnabled != 0 {
							t.Fatalf("old scan changed or queued: %s %d %v", state, historyEnabled, err)
						}
					} else if role == "control" {
						var content string
						var scheduled int
						if err := db.SQL.QueryRow("SELECT m.content,s.scheduled_job_id IS NOT NULL FROM agent_sessions s JOIN agent_messages m ON m.session_id=s.id JOIN agent_reports r ON r.message_id=m.id WHERE s.id='kept-session'").Scan(&content, &scheduled); err != nil || content != "existing report" || scheduled != 0 {
							t.Fatalf("report changed: %s %d %v", content, scheduled, err)
						}
					}
				})
			}
		}
		for _, role := range []string{"worker", "control"} {
			t.Run(entry+"/rollback/"+role, func(t *testing.T) {
				db := scheduledAnalysisOldDatabase(t, role, 38)
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
		t.Run(entry+"/unsupported", func(t *testing.T) {
			db := oldDatabase(t, "worker")
			if _, err := db.SQL.Exec("PRAGMA user_version=35"); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil || !strings.Contains(err.Error(), "unsupported database version") {
				t.Fatal("unsupported database accepted")
			}
			assertVersion(t, db, 35)
		})
	}
}
