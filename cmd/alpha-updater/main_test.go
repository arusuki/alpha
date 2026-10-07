package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/app"
	"project-alpha/internal/cluster"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

// Exercise the actual binary, including the inherited-lock release entry point.
func TestDatabaseUpgradeCommand(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build updater: %v\n%s", err, output)
	}
	help, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), fmt.Sprintf("目标数据库版本：%d", platform.DatabaseVersion)) {
		t.Fatalf("help schema: %v\n%s", err, help)
	}
	cases := []struct {
		role       string
		initialize func(*sql.Tx) error
		version    int
		downgrade  string
		tables     []string
		conflict   bool
	}{
		{"control", cluster.Initialize, 34, "DROP TABLE member_key_sync; DROP TABLE member_key_revocations", []string{"members", "member_invitations"}, false},
		{"worker", app.Initialize, 35, "DROP TABLE gpu_intervals", []string{"managed_containers", "owners", "member_container_slots"}, false},
		{"registry", registry.Initialize, 35, "", []string{"registry_control", "registry_sessions"}, false},
		{"worker", app.Initialize, 35, "DROP TABLE gpu_intervals; CREATE TABLE gpu_intervals_expiry(value TEXT); INSERT INTO gpu_intervals_expiry VALUES('preserve')", []string{"managed_containers", "owners", "member_container_slots", "gpu_intervals_expiry"}, true},
	}
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%s/conflict=%t", entry, tc.role, tc.conflict), func(t *testing.T) {
				db, err := platform.OpenDatabase(t.TempDir(), tc.initialize)
				if err != nil {
					t.Fatal(err)
				}
				defer db.SQL.Close()
				switch tc.role {
				case "control":
					store := &members.Store{Database: db}
					invite, err := store.CreateInvitation("upgrade", 1, "admin")
					if err != nil {
						t.Fatal(err)
					}
					key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
					_, err = store.RegisterWith(members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: key, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, nil)
					if err != nil {
						t.Fatal(err)
					}
				case "worker":
					_, err = db.SQL.Exec(`INSERT INTO managed_containers VALUES(?,'unix:///test.sock','daemon','training','alice','{"port":2222}','fingerprint','create','',1,123);
INSERT INTO member_container_slots(member_id,username,plan,deleted) VALUES('member','alice','{}',0)`, strings.Repeat("a", 64))
				case "registry":
					_, err = db.SQL.Exec(`INSERT INTO registry_control VALUES(1,'control');
INSERT INTO registry_sessions VALUES('token','invitation','csrf','resource','{}','pending-registration',0,1234567890)`)
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.downgrade != "" {
					if _, err = db.SQL.Exec(tc.downgrade); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", tc.version)); err != nil {
					t.Fatal(err)
				}
				read := func(query string) []map[string]any {
					t.Helper()
					rows, err := platform.Rows(db.SQL, query)
					if err != nil {
						t.Fatal(err)
					}
					return rows
				}
				tables := append([]string{"service_identity"}, tc.tables...)
				before := make(map[string][]map[string]any)
				for _, table := range tables {
					before[table] = read("SELECT * FROM " + table + " ORDER BY 1")
				}
				const schemaQuery = "SELECT type,name,sql FROM sqlite_master ORDER BY type,name"
				oldSchema := read(schemaQuery)
				oldAudit := read("SELECT * FROM audit ORDER BY id")
				args := []string{"--database-only", "--data-dir", db.Directory}
				var files []*os.File
				if entry == "_migrate" {
					lock, err := db.LockService()
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
					files = []*os.File{lock}
					args = []string{"_migrate", tc.role, db.Directory}
				}
				for range 2 {
					cmd := exec.Command(binary, args...)
					cmd.ExtraFiles = files
					output, err := cmd.CombinedOutput()
					if tc.conflict {
						if err == nil || !strings.Contains(string(output), "existing data preserved") {
							t.Fatalf("expected explicit migration failure: %v\n%s", err, output)
						}
					} else if err != nil {
						t.Fatalf("upgrade command: %v\n%s", err, output)
					}
				}
				wantVersion := platform.DatabaseVersion
				if tc.conflict {
					wantVersion = tc.version
					if !reflect.DeepEqual(oldSchema, read(schemaQuery)) || !reflect.DeepEqual(oldAudit, read("SELECT * FROM audit ORDER BY id")) {
						t.Fatal("failed migration changed schema or audit")
					}
				}
				var version int
				if err = db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != wantVersion {
					t.Fatalf("schema %d, want %d: %v", version, wantVersion, err)
				}
				for _, table := range tables {
					if !reflect.DeepEqual(before[table], read("SELECT * FROM "+table+" ORDER BY 1")) {
						t.Fatalf("migration changed existing %s data", table)
					}
				}
				if !tc.conflict {
					if tc.role == "control" {
						read("SELECT * FROM member_key_sync")
						read("SELECT * FROM member_key_revocations")
					}
					gpuSchema := read("SELECT name FROM sqlite_master WHERE name IN ('gpu_intervals','gpu_intervals_expiry')")
					if tc.role == "worker" {
						if len(gpuSchema) != 2 {
							t.Fatal("worker GPU table or expiry index missing")
						}
						if _, err := db.SQL.Exec("INSERT INTO gpu_intervals VALUES('GPU-test','Test GPU',1,16,80,'[\"alice\"]')"); err != nil {
							t.Fatal(err)
						}
					} else if len(gpuSchema) != 0 {
						t.Fatal("GPU history created for a non-worker")
					}
					if len(read("SELECT * FROM audit WHERE action='database.upgrade'")) != 1 {
						t.Fatal("repeat upgrade was not a no-op")
					}
					if entry == "--database-only" {
						backups, err := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
						if err != nil || len(backups) != 1 {
							t.Fatalf("expected one backup: %v %v", backups, err)
						}
					}
				}
			})
		}
	}
}
