package updater

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"project-alpha/internal/credentials"
	"project-alpha/internal/platform"
)

func TestAgentProviderBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, kind := range []string{"control", "worker", "worker-legacy", "registry"} {
			for _, version := range []int{42, 43, 44, 45, 46, 47} {
				for _, rollback := range []bool{false, true} {
					if rollback && version != 47 {
						continue
					}
					t.Run(fmt.Sprintf("%s/%s/%d/rollback=%v", entry, kind, version, rollback), func(t *testing.T) {
						role := kind
						if kind == "worker-legacy" {
							role = "worker"
						}
						db := memberAssistantOldDatabase(t, binary, entry, role, version)
						if kind == "worker-legacy" {
							if _, err := db.SQL.Exec(`CREATE TABLE agent_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL,api_key_ciphertext TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 1)`); err != nil {
								t.Fatal(err)
							}
						}
						hasSettings := kind == "control" || kind == "worker-legacy"
						const oldSettings = `{"protocol":"responses","endpoint":"https://model.example/v1","model":"kept-model","timeout_seconds":180,"reasoning_summary":true}`
						cipher := ""
						if hasSettings {
							var err error
							cipher, err = credentials.Encrypt(db.Directory, "agent-api-key.key", "project-alpha/agent-settings/api-key/v1", "kept-secret")
							if err != nil {
								t.Fatal(err)
							}
							if _, err = db.SQL.Exec("INSERT INTO agent_settings VALUES(1,?,?,13)", oldSettings, cipher); err != nil {
								t.Fatal(err)
							}
						}
						if rollback {
							if _, err := db.SQL.Exec(`CREATE TRIGGER fail_provider BEFORE INSERT ON audit WHEN NEW.action='database.upgrade' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
								t.Fatal(err)
							}
						}
						schema, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
						if err != nil {
							t.Fatal(err)
						}
						users, err := platform.Rows(db.SQL, "SELECT * FROM users")
						if err != nil {
							t.Fatal(err)
						}
						err = runScheduledMigration(t, binary, entry, role, db)
						if rollback {
							if err == nil {
								t.Fatal("failed migration did not exit nonzero")
							}
							assertVersion(t, db, version)
						} else {
							if err != nil {
								t.Fatal(err)
							}
							if err = runScheduledMigration(t, binary, entry, role, db); err != nil {
								t.Fatal("repeat migration", err)
							}
							assertVersion(t, db, platform.DatabaseVersion)
						}
						if version == 47 && (role != "control" || rollback) {
							after, e := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
							if e != nil || !reflect.DeepEqual(schema, after) {
								t.Fatal("provider migration changed table structure", e)
							}
						}
						afterUsers, err := platform.Rows(db.SQL, "SELECT * FROM users")
						if err != nil || !reflect.DeepEqual(users, afterUsers) {
							t.Fatal("existing users changed", err)
						}
						if hasSettings {
							var raw, afterCipher string
							var revision int
							if err = db.SQL.QueryRow("SELECT value,api_key_ciphertext,revision FROM agent_settings WHERE id=1").Scan(&raw, &afterCipher, &revision); err != nil {
								t.Fatal(err)
							}
							var before, after map[string]any
							if json.Unmarshal([]byte(oldSettings), &before) != nil || json.Unmarshal([]byte(raw), &after) != nil {
								t.Fatal("invalid settings JSON")
							}
							wantRevision := 13
							if !rollback {
								before["provider"] = "openai"
								wantRevision++
							}
							if !reflect.DeepEqual(before, after) || afterCipher != cipher || revision != wantRevision {
								t.Fatal("settings/credential preservation or repeat migration failed")
							}
						}
					})
				}
			}
		}
	}
}
