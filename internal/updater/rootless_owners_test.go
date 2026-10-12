package updater

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestRootlessOwnersBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"worker", "control", "registry"} {
			for _, version := range []int{39, 40, 41, 42, 43, 44} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := nodeServicesOldDatabase(t, role, min(version, 41))
					if version >= 42 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
						if role == "worker" {
							if _, err := db.SQL.Exec("DROP TABLE rootless_container_owners"); err != nil {
								t.Fatal(err)
							}
						}
						if role == "control" && version < 44 {
							if _, err := db.SQL.Exec("DROP TABLE assistant_settings"); err != nil {
								t.Fatal(err)
							}
						}
						if role == "control" && version < 43 {
							if _, err := db.SQL.Exec("DROP TABLE assistant_sessions; DROP TABLE assistant_memories"); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
							t.Fatal(err)
						}
					}
					if role == "worker" {
						if _, err := db.SQL.Exec(`INSERT INTO gpu_intervals VALUES('GPU-kept','GPU',100,115,45,'["alice"]')`); err != nil {
							t.Fatal(err)
						}
					}
					before, err := platform.Rows(db.SQL, "SELECT * FROM users")
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
					}
					assertVersion(t, db, platform.DatabaseVersion)
					after, err := platform.Rows(db.SQL, "SELECT * FROM users")
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatal("users changed", err)
					}
					var count int
					if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='rootless_container_owners'").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if role != "worker" {
						if count != 0 {
							t.Fatal("worker table on", role)
						}
						return
					}
					if count != 1 {
						t.Fatal("missing table")
					}
					for _, id := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
						if _, err := db.SQL.Exec("INSERT INTO rootless_container_owners VALUES(?,'kept-container',123,'admin')", id); err != nil {
							t.Fatal(err)
						}
					}
					var owner string
					if err := db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id='kept-container'").Scan(&owner); err != nil || owner != "alice" {
						t.Fatal("primary changed", owner, err)
					}
					if _, err := db.SQL.Exec("INSERT INTO owners VALUES('another-primary','alice')"); err == nil {
						t.Fatal("primary one-to-one constraint lost")
					}
					var tags string
					if err := db.SQL.QueryRow("SELECT owners FROM gpu_intervals WHERE gpu_uuid='GPU-kept' AND started_at=100 AND ended_at=115 AND utilization=45").Scan(&tags); err != nil || tags != `["alice"]` {
						t.Fatal("GPU history changed", tags, err)
					}
				})
			}
		}
		t.Run(entry+"/rollback", func(t *testing.T) {
			db := nodeServicesOldDatabase(t, "worker", 41)
			if err := runScheduledMigration(t, binary, entry, "worker", db); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL.Exec("DROP TABLE rootless_container_owners; PRAGMA user_version=44; CREATE TRIGGER fail_rootless BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'failure'); END"); err != nil {
				t.Fatal(err)
			}
			before, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
			if err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil {
				t.Fatal("failure ignored")
			}
			assertVersion(t, db, 44)
			after, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("schema changed after failure", err)
			}
			var name string
			if err := db.SQL.QueryRow("SELECT name FROM managed_containers WHERE id='kept-container'").Scan(&name); err != nil || name != "kept-name" {
				t.Fatal("data lost", err)
			}
		})
	}
}
