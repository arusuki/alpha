package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestRootlessOwnersCLI(t *testing.T) {
	dir := t.TempDir()
	db, err := platform.OpenDatabase(filepath.Join(dir, "data"), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	parent, a, b := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if _, err := db.SQL.Exec("INSERT INTO owners VALUES(?,'alice')", parent); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nshift 2\ncase \"$1\" in\ninfo) printf '%s\\n' '[\"name=rootless\"]';;\nps) printf '%s\\n' '" + a + "' '" + b + "';;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	run := func(args ...string) error {
		return Run(context.Background(), append([]string{"containers", "rootless-owner", "--data-dir", db.Directory}, args...))
	}
	for _, tc := range []struct {
		args  []string
		count int
	}{
		{[]string{"--owner", "alice", "--all", "--dry-run"}, 0},
		{[]string{"--owner", "alice", "--all"}, 2},
		{[]string{"--list"}, 2},
		{[]string{"--clear", a}, 1},
	} {
		if err := run(tc.args...); err != nil {
			t.Fatal(tc, err)
		}
		var count int
		if err := db.SQL.QueryRow("SELECT count(*) FROM rootless_container_owners").Scan(&count); err != nil || count != tc.count {
			t.Fatal(count, err)
		}
	}
	if _, err := db.SQL.Exec("INSERT INTO owners VALUES(?,'alice')", strings.Repeat("d", 64)); err == nil {
		t.Fatal("lost primary uniqueness")
	}
	if _, err := db.SQL.Exec("PRAGMA user_version=44"); err != nil {
		t.Fatal(err)
	}
	if err := run("--owner", "alice", "--all"); err == nil || !strings.Contains(err.Error(), "alpha-updater") {
		t.Fatal("old version accepted", err)
	}
	var version int
	if err := db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 44 {
		t.Fatal("CLI migrated data", version, err)
	}
}

func TestRootlessSnapshotDoesNotConsumePrimarySlot(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	parent, child := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := db.SQL.Exec(`INSERT INTO owners VALUES(?,'alice');
 INSERT INTO rootless_container_owners VALUES(?,?,1,'admin');
 INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES('job','completed','manual','admin',1,'{}');
 INSERT INTO snapshot_records VALUES('job',1,'/',?)`, parent, child, parent, `{"containers":[{"id":"`+child+`","rootless":true,"owner":"alice"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *sql.Tx) error { return platform.CheckContainerOwner(tx, parent, "alice") }); err != nil {
		t.Fatal("child consumed primary quota", err)
	}
	if err := db.Transaction(func(tx *sql.Tx) error { return newContainerHandler(db).UnassignOwner(tx, "alice") }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.SQL.QueryRow("SELECT count(*) FROM owners").Scan(&count); err != nil || count != 1 {
		t.Fatal("child entered primary owners", count, err)
	}
}
