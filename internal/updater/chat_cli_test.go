package updater

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"project-alpha/internal/platform"
)

func dropChatCLISchema(t *testing.T, db *platform.Database, role string) {
	t.Helper()
	query := map[string]string{"control": "DROP TABLE member_cli_sync; DROP TABLE chat_cli_settings", "worker": "DROP TABLE container_cli_installs; DROP TABLE member_cli_configs"}[role]
	if query != "" {
		if _, err := db.SQL.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChatCLIBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"control", "worker", "registry"} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/rollback=%v", entry, role, fail), func(t *testing.T) {
					db := memberAssistantOldDatabase(t, binary, entry, role, 46)
					if role == "control" {
						if _, err := db.SQL.Exec(`INSERT INTO member_assistant_sessions VALUES('chat','kept-member','title','{}','[]',1,2); INSERT INTO member_assistant_memories VALUES('kept-member','[{"memory":"keep"}]',2)`); err != nil {
							t.Fatal(err)
						}
					}
					if fail {
						if _, err := db.SQL.Exec(`CREATE TRIGGER fail_cli BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
							t.Fatal(err)
						}
					}
					schema, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
					if err != nil {
						t.Fatal(err)
					}
					before := map[string][]map[string]any{}
					for _, row := range schema {
						if row["type"] != "table" || (row["name"] == "audit" || row["name"] == "sqlite_sequence") {
							continue
						}
						name := row["name"].(string)
						before[name], err = platform.Rows(db.SQL, `SELECT * FROM "`+name+`"`)
						if err != nil {
							t.Fatal(err)
						}
					}
					err = runScheduledMigration(t, binary, entry, role, db)
					if fail {
						if err == nil {
							t.Fatal("migration unexpectedly succeeded")
						}
						assertVersion(t, db, 46)
						after, e := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
						if e != nil || !reflect.DeepEqual(schema, after) {
							t.Fatal("rollback changed schema", e)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if err = runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
						assertVersion(t, db, platform.DatabaseVersion)
						var count int
						if err = db.SQL.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('chat_cli_settings','member_cli_sync','member_cli_configs','container_cli_installs')`).Scan(&count); err != nil {
							t.Fatal(err)
						}
						want := 2
						if role == "registry" {
							want = 0
						}
						if count != want {
							t.Fatal("wrong role schema", count)
						}
						if role == "control" {
							var path string
							if err = db.SQL.QueryRow("SELECT command_path FROM chat_cli_settings WHERE id=1").Scan(&path); err != nil || path != "/usr/local/bin/alpha-chat" {
								t.Fatal(path, err)
							}
						}
					}
					for name, rows := range before {
						after, e := platform.Rows(db.SQL, `SELECT * FROM "`+name+`"`)
						if e != nil || !reflect.DeepEqual(rows, after) {
							t.Fatal("existing data changed", name, e)
						}
					}
				})
			}
		}
	}
}
