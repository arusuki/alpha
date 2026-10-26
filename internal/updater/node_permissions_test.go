package updater

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"project-alpha/internal/platform"
)

func dropNodePermissionsSchema(t *testing.T, db *platform.Database, role string) {
	t.Helper()
	if role == "control" {
		if _, err := db.SQL.Exec("DROP TABLE member_node_permissions; DROP TABLE invitation_node_permissions"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodePermissionsBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"control", "worker", "registry"} {
			for _, version := range []int{42, 43, 44, 45, 46, 47, 48} {
				for _, rollback := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/rollback=%v", entry, role, version, rollback), func(t *testing.T) {
						db := memberAssistantOldDatabase(t, binary, entry, role, version)
						if role == "control" {
							if _, err := db.SQL.Exec(`INSERT INTO member_invitations VALUES('kept-invite','code-hash','cipher','cohort',10,1,0,'admin',123);
INSERT INTO cluster_nodes VALUES('worker','kept-worker','http://worker','token',1,'worker','10.0.0.1');
INSERT INTO member_node_resources VALUES('kept-member','worker','ready','kept-container','workspace',2222,'',123,'create','');`); err != nil {
								t.Fatal(err)
							}
						}
						if rollback {
							if _, err := db.SQL.Exec(`CREATE TRIGGER fail_permissions BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
								t.Fatal(err)
							}
						}
						schema, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
						if err != nil {
							t.Fatal(err)
						}
						before := map[string][]map[string]any{}
						for _, row := range schema {
							if row["type"] != "table" || row["name"] == "audit" || row["name"] == "sqlite_sequence" {
								continue
							}
							name := row["name"].(string)
							before[name], err = platform.Rows(db.SQL, `SELECT * FROM "`+name+`"`)
							if err != nil {
								t.Fatal(err)
							}
						}
						err = runScheduledMigration(t, binary, entry, role, db)
						if rollback {
							if err == nil {
								t.Fatal("failed migration exited successfully")
							}
							assertVersion(t, db, version)
							after, e := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
							if e != nil || !reflect.DeepEqual(schema, after) {
								t.Fatal("rollback changed schema", e)
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							if err = runScheduledMigration(t, binary, entry, role, db); err != nil {
								t.Fatal("repeat", err)
							}
							assertVersion(t, db, platform.DatabaseVersion)
							var count int
							if err = db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('member_node_permissions','invitation_node_permissions')").Scan(&count); err != nil {
								t.Fatal(err)
							}
							if role == "control" {
								if count != 2 {
									t.Fatal("missing permissions schema")
								}
								for _, table := range []string{"member_node_permissions", "invitation_node_permissions"} {
									var raw string
									if err = db.SQL.QueryRow("SELECT permissions FROM " + table).Scan(&raw); err != nil || raw != `{"all_nodes":true,"node_ids":[]}` {
										t.Fatal("existing permission not preserved", raw, err)
									}
								}
							} else if count != 0 {
								t.Fatal("wrong role schema")
							}
						}
						for table, rows := range before {
							after, e := platform.Rows(db.SQL, `SELECT * FROM "`+table+`"`)
							if e != nil || !reflect.DeepEqual(rows, after) {
								t.Fatal("existing data changed", table, e)
							}
						}
					})
				}
			}
		}
	}
}
