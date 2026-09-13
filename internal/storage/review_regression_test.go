package storage

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type failingMountReader struct{}

func (failingMountReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestMountTableLongLineAndReadError(t *testing.T) {
	first := "1 0 8:1 / / rw - overlay overlay lowerdir=" + strings.Repeat("x", 70000) + "\n"
	second := "2 1 8:2 / /a\\040b rw - ext4 /dev/sdb rw"
	mounts, err := parseMountTable(strings.NewReader(first + second))
	if err != nil || len(mounts) != 2 || mounts[0].FS != "overlay" || mounts[1].Path != "/a b" {
		t.Fatalf("long mountinfo: %+v %v", mounts, err)
	}
	mounts, err = parseMountTable(io.MultiReader(strings.NewReader(first), failingMountReader{}))
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(mounts) != 1 {
		t.Fatalf("lost read error: %+v %v", mounts, err)
	}
}

func TestScanCLIRejectsInvalidConfigBeforeScanning(t *testing.T) {
	for _, args := range [][]string{
		{"--max-nodes", "1"}, {"--docker-timeout", "1"}, {"--max-depth", "33"}, {"--max-nodes", "100001"}, {"--docker-timeout", "3601"},
		{"--root", "relative"}, {"--exclude", "relative"}, {"--owner-label", "invalid label"}, {"--no-docker"},
	} {
		if err := ScanCLI(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

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
	a := &AgentManager{db: db, active: "session", pending: &agentCompletion{id: "session", status: "cancelled", message: "stopped"}}
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
