package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/cluster"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

// Exercise the real command, including the inherited-lock entry point invoked
// by release installation, rather than only calling the migration function.
func TestDatabaseUpgradeCommandPreservesMemberKeys(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build updater: %v\n%s", err, output)
	}
	help, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), fmt.Sprintf("目标数据库版本：%d", platform.DatabaseVersion)) {
		t.Fatalf("help schema: %v\n%s", err, help)
	}
	for _, entry := range []string{"--database-only", "_migrate"} {
		t.Run(entry, func(t *testing.T) {
			db, err := platform.OpenDatabase(t.TempDir(), cluster.Initialize)
			if err != nil {
				t.Fatal(err)
			}
			defer db.SQL.Close()
			store := &members.Store{Database: db}
			invite, err := store.CreateInvitation("upgrade", 1, "admin")
			if err != nil {
				t.Fatal(err)
			}
			// Existing public fixture; no test keys are generated.
			key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
			_, err = store.RegisterWith(members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: key, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			before, err := platform.Rows(db.SQL, "SELECT * FROM members")
			if err != nil {
				t.Fatal(err)
			}
			// Reconstruct schema 34, whose member data used a single public-key line.
			if _, err = db.SQL.Exec("DROP TABLE member_key_sync; DROP TABLE member_key_revocations; PRAGMA user_version=34"); err != nil {
				t.Fatal(err)
			}
			args := []string{"--database-only", "--data-dir", db.Directory}
			var files []*os.File
			if entry == "_migrate" {
				lock, err := db.LockService()
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				files = []*os.File{lock}
				args = []string{"_migrate", "control", db.Directory}
			}
			// Repeat the command to verify that a completed migration is idempotent.
			for range 2 {
				cmd := exec.Command(binary, args...)
				cmd.ExtraFiles = files
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("upgrade command: %v\n%s", err, output)
				}
			}
			var version int
			if err = db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != platform.DatabaseVersion {
				t.Fatalf("schema %d: %v", version, err)
			}
			for _, table := range []string{"member_key_sync", "member_key_revocations"} {
				if _, err = platform.Rows(db.SQL, "SELECT * FROM "+table); err != nil {
					t.Fatal(err)
				}
			}
			after, err := platform.Rows(db.SQL, "SELECT * FROM members")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("member data changed during upgrade: %v", err)
			}
			if entry == "--database-only" {
				backups, err := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
				if err != nil || len(backups) != 1 {
					t.Fatalf("expected one backup: %v %v", backups, err)
				}
			}
		})
	}
}
