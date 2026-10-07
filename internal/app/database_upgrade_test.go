package app

import (
	"database/sql"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestWorkerUpgradePreservesRecordsAndRejectsConflicts(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "conflict"}[conflict], func(t *testing.T) {
			dir := t.TempDir()
			db, err := platform.OpenDatabase(dir, Initialize)
			if err != nil {
				t.Fatal(err)
			}
			id, cid := strings.Repeat("1", 32), strings.Repeat("a", 64)
			// Reconstruct the previous worker schema without invoking current writers.
			err = db.Transaction(func(tx *sql.Tx) error {
				triggers, err := platform.Rows(tx, "SELECT name FROM sqlite_master WHERE type='trigger'")
				if err != nil {
					return err
				}
				for _, r := range triggers {
					if _, err = tx.Exec("DROP TRIGGER " + r["name"].(string)); err != nil {
						return err
					}
				}
				if _, err = tx.Exec(`DROP TABLE gpu_intervals; DROP INDEX member_slot_container; DROP INDEX managed_container_owner; DROP INDEX owners_one_container;
 ALTER TABLE member_container_slots DROP COLUMN container_id; ALTER TABLE member_container_slots DROP COLUMN mode; PRAGMA user_version=33;`); err != nil {
					return err
				}
				if _, err = tx.Exec("INSERT INTO managed_containers VALUES(?,'unix:///test.sock','daemon',?,'alice','{\"port\":2222}','fingerprint','create','',1,123)", cid, "alpha-"+id); err != nil {
					return err
				}
				if _, err = tx.Exec("INSERT INTO member_container_slots(member_id,username,plan,deleted) VALUES(?,'alice','{}',0)", id); err != nil {
					return err
				}
				if conflict {
					_, err = tx.Exec("INSERT INTO owners VALUES('another','alice')")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			db.SQL.Close()
			upgraded, err := platform.OpenDatabase(dir, Initialize)
			if conflict {
				if err == nil {
					upgraded.SQL.Close()
					t.Fatal("silently rewrote duplicate ownership")
				}
				raw, e := sql.Open("sqlite3", dir+"/platform.sqlite3")
				if e != nil {
					t.Fatal(e)
				}
				defer raw.Close()
				var version int
				raw.QueryRow("PRAGMA user_version").Scan(&version)
				if version != 33 {
					t.Fatal("failed upgrade changed version")
				}
				var name string
				if e = raw.QueryRow("SELECT name FROM managed_containers WHERE id=?", cid).Scan(&name); e != nil || name != "alpha-"+id {
					t.Fatal("failed upgrade lost container")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.SQL.Close()
			var target, mode, fp string
			if err = upgraded.SQL.QueryRow("SELECT container_id,mode FROM member_container_slots WHERE member_id=?", id).Scan(&target, &mode); err != nil || target != cid || mode != "create" {
				t.Fatalf("upgrade lost slot: %s %s %v", target, mode, err)
			}
			if err = upgraded.SQL.QueryRow("SELECT fingerprint FROM managed_containers WHERE id=?", cid).Scan(&fp); err != nil || fp != "fingerprint" {
				t.Fatal("changed identity")
			}
		})
	}
}
