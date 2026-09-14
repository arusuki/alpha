package agent

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAgentCompletionSurvivesSQLiteBusy(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err := db.SQL.Exec("INSERT INTO users VALUES('user','administrator','unused','admin',1,0); INSERT INTO agent_sessions VALUES('session','user','title','running',0,0,NULL,'job',NULL,'completions','test'); PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	blocker, err := sql.Open("sqlite3", filepath.Join(db.Directory, "platform.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec("ROLLBACK")
	a := &Manager{db: NewStore(db), active: "session", pending: &agentCompletion{id: "session", status: "cancelled", message: "stopped"}}
	if err := a.flushCompletion(); err == nil {
		t.Fatal("expected SQLITE_BUSY")
	}
	if a.active != "session" || a.pending == nil {
		t.Fatal("busy write discarded completion")
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := a.flushCompletion(); err != nil {
		t.Fatal(err)
	}
	var status, message string
	var active sql.NullString
	if err := db.SQL.QueryRow("SELECT status,error,active_job_id FROM agent_sessions WHERE id='session'").Scan(&status, &message, &active); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || message != "stopped" || active.Valid || a.active != "" || a.pending != nil {
		t.Fatal("completion did not recover after SQLite lock released")
	}
}
