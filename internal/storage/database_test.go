package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestDatabaseInitializationAndReopen(t *testing.T) {
	directory := t.TempDir()
	db, err := openDatabase(directory)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != platform.DatabaseVersion {
		t.Fatalf("database version: %d, %v", version, err)
	}
	settings, err := db.config()
	if err != nil || httpapi.JSONText(settings.Value) != httpapi.JSONText(defaultConfig()) {
		t.Fatalf("initial settings: %+v, %v", settings, err)
	}
	settings.Value.ScanMode = "fast"
	if _, err := db.saveConfig(settings.Value, settings.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDatabase(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	saved, err := db.config()
	if err != nil || saved.Revision != settings.Revision+1 || saved.Value.ScanMode != "fast" {
		t.Fatalf("reopening changed saved settings: %+v, %v", saved, err)
	}
}

func TestDatabaseRejectsUnsupportedVersions(t *testing.T) {
	for _, version := range []int{1, platform.DatabaseVersion - 1, platform.DatabaseVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			directory := t.TempDir()
			raw, err := sql.Open("sqlite3", filepath.Join(directory, "platform.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if db, err := openDatabase(directory); err == nil {
				db.SQL.Close()
				t.Fatal("opened an unsupported database")
			}
			var actual, tables int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != version {
				t.Fatalf("rejection changed the version: %d, %v", actual, err)
			}
			if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil || tables != 0 {
				t.Fatalf("rejection created tables: %d, %v", tables, err)
			}
		})
	}
}

func TestDatabaseInitializationRollsBack(t *testing.T) {
	directory := t.TempDir()
	raw, err := sql.Open("sqlite3", filepath.Join(directory, "platform.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// An unversioned, nonempty database must not be partially initialized.
	if _, err := raw.Exec("CREATE TABLE jobs (value TEXT); INSERT INTO jobs VALUES('keep')"); err != nil {
		t.Fatal(err)
	}
	if db, err := openDatabase(directory); err == nil {
		db.SQL.Close()
		t.Fatal("initialized over existing tables")
	}
	var version, tables int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("failed initialization changed version: %d, %v", version, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("failed initialization left tables: %d, %v", tables, err)
	}
	var value string
	if err := raw.QueryRow("SELECT value FROM jobs").Scan(&value); err != nil || value != "keep" {
		t.Fatalf("failed initialization changed existing data: %q, %v", value, err)
	}
}

func TestDatabaseUsesHotQueryIndexes(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	for query, index := range map[string]string{
		"SELECT id FROM jobs WHERE status='completed' AND trigger<>'incremental' ORDER BY finished_at DESC LIMIT 1": "idx_jobs_latest",
		"SELECT path FROM snapshot_changes WHERE job_id='job' AND revision>1":                                       "idx_snapshot_changes_revision",
	} {
		plan, err := platform.Rows(db.SQL, "EXPLAIN QUERY PLAN "+query)
		if err != nil {
			t.Fatal(err)
		}
		if text := httpapi.JSONText(plan); !strings.Contains(text, index) || strings.Contains(text, "TEMP B-TREE") {
			t.Fatalf("query does not use index %s: %s", index, text)
		}
	}
}

func TestSnapshotBranchUsesParentLookup(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	// Both publication and SSE/polling must seek children by job and parent,
	// rather than rescan the entire record for every node in the branch.
	for _, suffix := range []string{
		" SELECT path FROM branch",
		" SELECT n.path,n.parent,n.value FROM snapshot_nodes n JOIN branch b ON n.path=b.path WHERE n.job_id=? ORDER BY n.parent,n.position",
	} {
		args := []any{"job", "/tmp", "job"}
		if strings.Contains(suffix, "?") {
			args = append(args, "job")
		}
		plan, err := platform.Rows(db.SQL, "EXPLAIN QUERY PLAN "+snapshotBranchCTE+suffix, args...)
		if err != nil {
			t.Fatal(err)
		}
		if text := httpapi.JSONText(plan); !strings.Contains(text, "snapshot_node_parents (job_id=? AND parent=?)") {
			t.Fatalf("recursive traversal does not seek by parent: %s", text)
		}
	}
}
