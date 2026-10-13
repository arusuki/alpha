package updater

import (
	"fmt"
	"path/filepath"
	"testing"

	"project-alpha/internal/platform"
)

// Both entry points execute the actual built updater, retaining prior role data.
func TestAssistantBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"control", "worker", "registry"} {
			for _, version := range []int{41, 42, 43} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := dramMountOldDatabase(t, role)
					if version >= 42 {
						// Upgrade through the shared chain, then remove only the new schema to
						// reproduce the immediately preceding release for every role.
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
						dropChatCLISchema(t, db, role)
						if role == "control" {
							if _, err := db.SQL.Exec("DROP TABLE member_assistant_sessions; DROP TABLE member_assistant_memories"); err != nil {
								t.Fatal(err)
							}
						}
						if role == "worker" {
							if _, err := db.SQL.Exec("DROP TABLE rootless_container_owners"); err != nil {
								t.Fatal(err)
							}
						}
						if role == "control" {
							if _, err := db.SQL.Exec("DROP TABLE assistant_settings"); err != nil {
								t.Fatal(err)
							}
							if version == 42 {
								if _, err := db.SQL.Exec("DROP TABLE assistant_sessions; DROP TABLE assistant_memories"); err != nil {
									t.Fatal(err)
								}
							}
						}
						if _, err := db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
							t.Fatal(err)
						}
					}
					if version == 43 && role == "control" {
						if _, err := db.SQL.Exec(`INSERT INTO assistant_sessions VALUES('kept-assistant','user','旧对话','{"kept":true}','[]',1,2); INSERT INTO assistant_memories VALUES('user','[{"memory":"先看 GPU"}]',2)`); err != nil {
							t.Fatal(err)
						}
					}
					for range 2 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
					}
					assertVersion(t, db, platform.DatabaseVersion)
					if role == "control" {
						var prompt string
						if err := db.SQL.QueryRow("SELECT json_extract(value,'$.instructions') FROM assistant_settings WHERE id=1").Scan(&prompt); err != nil || prompt == "" {
							t.Fatal("missing settings default", err)
						}
						if version == 43 {
							var data string
							if err := db.SQL.QueryRow("SELECT data FROM assistant_memories WHERE user_id='user'").Scan(&data); err != nil || data != `[{"memory":"先看 GPU"}]` {
								t.Fatal("memory lost", data, err)
							}
							if err := db.SQL.QueryRow("SELECT data FROM assistant_sessions WHERE id='kept-assistant'").Scan(&data); err != nil || data != `{"kept":true}` {
								t.Fatal("session lost", data, err)
							}
						}
					}

					var tables int
					if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('assistant_sessions','assistant_memories','assistant_settings')").Scan(&tables); err != nil {
						t.Fatal(err)
					}
					if (role == "control" && tables != 3) || (role != "control" && tables != 0) {
						t.Fatal("wrong role schema", tables)
					}
					if role == "control" {
						var content string
						if err := db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id='kept-session'").Scan(&content); err != nil || content != "existing report" {
							t.Fatal("report not preserved", content, err)
						}
					}
				})
			}
		}
		t.Run(entry+"/settings-rollback", func(t *testing.T) {
			db := dramMountOldDatabase(t, "control")
			if err := runScheduledMigration(t, binary, entry, "control", db); err != nil {
				t.Fatal(err)
			}
			dropChatCLISchema(t, db, "control")
			if _, err := db.SQL.Exec(`DROP TABLE member_assistant_sessions; DROP TABLE member_assistant_memories; DROP TABLE assistant_settings; PRAGMA user_version=43; INSERT INTO assistant_memories VALUES('user','[{"memory":"preserved"}]',2); CREATE TRIGGER fail_settings BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "control", db); err == nil {
				t.Fatal("expected rollback")
			}
			assertVersion(t, db, 43)
			var count int
			if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='assistant_settings'").Scan(&count); err != nil || count != 0 {
				t.Fatal("partial settings", err)
			}
			var memory string
			if err := db.SQL.QueryRow("SELECT data FROM assistant_memories WHERE user_id='user'").Scan(&memory); err != nil || memory != `[{"memory":"preserved"}]` {
				t.Fatal("rollback lost memory", err)
			}
		})
		t.Run(entry+"/rollback", func(t *testing.T) {
			db := populatedOldDatabase(t, "control")
			if _, err := db.SQL.Exec("CREATE TRIGGER reject_assistant BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'injected failure'); END"); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "control", db); err == nil {
				t.Fatal("migration must fail")
			}
			assertVersion(t, db, 41)
			var tables int
			if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('assistant_sessions','assistant_memories','assistant_settings')").Scan(&tables); err != nil || tables != 0 {
				t.Fatal("partial schema retained", tables, err)
			}
			var content string
			if err := db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id='kept-session'").Scan(&content); err != nil || content != "existing report" {
				t.Fatal(content, err)
			}
		})
	}
}
