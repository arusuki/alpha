package updater

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"project-alpha/internal/platform"
)

func memberAssistantOldDatabase(t *testing.T, binary, entry, role string, version int) *platform.Database {
	t.Helper()
	db := nodeServicesOldDatabase(t, role, min(version, 41))
	if version >= 42 {
		if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
			t.Fatal(err)
		}
		dropChatCLISchema(t, db, role)
		if role == "control" {
			if version < 46 {
				if _, err := db.SQL.Exec("DROP TABLE member_assistant_sessions; DROP TABLE member_assistant_memories"); err != nil {
					t.Fatal(err)
				}
			}
			if version < 44 {
				if _, err := db.SQL.Exec("DROP TABLE assistant_settings"); err != nil {
					t.Fatal(err)
				}
			}
			if version < 43 {
				if _, err := db.SQL.Exec("DROP TABLE assistant_sessions; DROP TABLE assistant_memories"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if role == "worker" && version < 45 {
			if _, err := db.SQL.Exec("DROP TABLE rootless_container_owners"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
			t.Fatal(err)
		}
	}
	if role == "control" {
		if _, err := db.SQL.Exec(`INSERT INTO members VALUES('kept-member','alice','{}','{}','[]','hash','cipher','existing-public-key','resource-hash','active','invite','invite-hash',123);
INSERT INTO member_sessions VALUES('session-hash','kept-member',9999999999);`); err != nil {
			t.Fatal(err)
		}
		if version >= 43 {
			if _, err := db.SQL.Exec(`INSERT INTO assistant_sessions VALUES('old-chat','user','title','{}','[]',1,2);INSERT INTO assistant_memories VALUES('user','[{"memory":"kept"}]',1)`); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func TestMemberAssistantBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"control", "worker", "registry"} {
			for _, version := range []int{39, 40, 41, 42, 43, 44, 45} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := memberAssistantOldDatabase(t, binary, entry, role, version)
					tables := []string{"users"}
					if role == "control" {
						tables = append(tables, "members", "member_sessions", "agent_messages")
						if version >= 43 {
							tables = append(tables, "assistant_sessions", "assistant_memories")
						}
					}
					before := map[string][]map[string]any{}
					// JSON rows retain all existing values, including password hashes and sessions.
					for _, table := range tables {
						rows, err := platform.Rows(db.SQL, "SELECT * FROM "+table)
						if err != nil {
							t.Fatal(err)
						}
						before[table] = rows
					}
					for range 2 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
					}
					assertVersion(t, db, platform.DatabaseVersion)
					for _, table := range tables {
						rows, err := platform.Rows(db.SQL, "SELECT * FROM "+table)
						if err != nil || !reflect.DeepEqual(before[table], rows) {
							t.Fatal("data changed", table, err)
						}
					}
					var count int
					if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('member_assistant_sessions','member_assistant_memories')").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if (role == "control" && count != 2) || (role != "control" && count != 0) {
						t.Fatal("wrong role schema", role, count)
					}
					if role == "control" {
						if _, err := db.SQL.Exec(`INSERT INTO member_assistant_sessions VALUES('chat','kept-member','title','{}','[]',1,2);INSERT INTO member_assistant_memories VALUES('kept-member','[]',1)`); err != nil {
							t.Fatal(err)
						}
						if _, err := db.SQL.Exec(`INSERT INTO member_assistant_memories VALUES('missing','[]',1)`); err == nil {
							t.Fatal("missing member accepted")
						}
					}
				})
			}
			t.Run(entry+"/"+role+"/rollback", func(t *testing.T) {
				db := memberAssistantOldDatabase(t, binary, entry, role, 45)
				if _, err := db.SQL.Exec(`CREATE TRIGGER fail_member_assistant BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
					t.Fatal(err)
				}
				before, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
				if err != nil {
					t.Fatal(err)
				}
				users, err := platform.Rows(db.SQL, "SELECT * FROM users")
				if err != nil {
					t.Fatal(err)
				}
				if err := runScheduledMigration(t, binary, entry, role, db); err == nil {
					t.Fatal("expected nonzero failure")
				}
				assertVersion(t, db, 45)
				after, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("partial schema committed", err)
				}
				afterUsers, err := platform.Rows(db.SQL, "SELECT * FROM users")
				if err != nil || !reflect.DeepEqual(users, afterUsers) {
					t.Fatal("failure changed data", err)
				}
			})
		}
	}
}
