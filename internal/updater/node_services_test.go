package updater

import (
	"path/filepath"
	"testing"

	"project-alpha/internal/platform"
)

// Start from the real retained tag's schema, including its DRAM parameters.
func dramMountOldDatabase(t *testing.T, role string) *platform.Database {
	t.Helper()
	db := populatedOldDatabase(t, role)
	if role == "worker" {
		if _, err := db.SQL.Exec(`INSERT INTO managed_containers VALUES('kept-container','unix:///test.sock','daemon','kept-name','alice','{}','kept-fingerprint','adopt','',1,123);
INSERT INTO node_service_settings VALUES('dram-bw','{"restart_policy":"on-failure","stop_timeout":35,"control_socket":"","socket_path":"","users":[],"dram":{"backend":"mock","interval_us":250000,"peak_gbps":256}}');
INSERT INTO node_service_settings VALUES('rootless-docker','{"restart_policy":"always","stop_timeout":150,"control_socket":"/run/custom/control.sock","socket_path":"/run/custom/docker.sock","users":["alice"]}');
INSERT INTO node_service_mounts(container_id,endpoint,daemon,owner,name,socket_path) VALUES('kept-container','unix:///test.sock','daemon','alice','kept-name','/run/kept.sock');`); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func TestDRAMMountsBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"worker", "control", "registry"} {
			t.Run(entry+"/"+role, func(t *testing.T) {
				db := dramMountOldDatabase(t, role)
				for range 2 {
					if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
						t.Fatal(err)
					}
					assertVersion(t, db, platform.DatabaseVersion)
				}
				if role == "worker" {
					var root, owner, state string
					var enabled, interval, revision, historyEnabled int
					if err := db.SQL.QueryRow("SELECT json_extract(value,'$.root[0]'),json_extract(value,'$.auto_agent_analyze'),json_extract(value,'$.interval_minutes'),revision,analysis_user_id FROM settings").Scan(&root, &enabled, &interval, &revision, &owner); err != nil || root != "/kept" || enabled != 0 || interval != 60 || owner != "" || revision < 13 {
						t.Fatalf("settings changed: %s %d %d %d %s: %v", root, enabled, interval, revision, owner, err)
					}
					if err := db.SQL.QueryRow("SELECT analysis_status,json_extract(config,'$.auto_agent_analyze') FROM jobs WHERE id='kept-scan'").Scan(&state, &historyEnabled); err != nil || state != "" || historyEnabled != 0 {
						t.Fatalf("old scan changed or queued: %s %d %v", state, historyEnabled, err)
					}
				} else if role == "control" {
					var content string
					var scheduled int
					if err := db.SQL.QueryRow("SELECT m.content,s.scheduled_job_id IS NOT NULL FROM agent_sessions s JOIN agent_messages m ON m.session_id=s.id JOIN agent_reports r ON r.message_id=m.id WHERE s.id='kept-session'").Scan(&content, &scheduled); err != nil || content != "existing report" || scheduled != 0 {
						t.Fatalf("report changed: %s %d %v", content, scheduled, err)
					}
				}
				var tables int
				if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('node_service_settings','node_service_mounts')").Scan(&tables); err != nil {
					t.Fatal(err)
				}
				if role == "worker" {
					if tables != 2 {
						t.Fatal("missing service tables", tables)
					}
					var socket, control string
					if err := db.SQL.QueryRow(`SELECT json_extract(config,'$.control_socket'),json_extract(config,'$.socket_path') FROM node_service_settings WHERE name='dram-bw'`).Scan(&control, &socket); err != nil || control != "/run/dram-bw-manager/control.sock" || socket != "/run/dram-bw/control.sock" {
						t.Fatal(control, socket, err)
					}
					var service, backend string
					if err := db.SQL.QueryRow("SELECT service FROM node_service_mounts WHERE socket_path='/run/kept.sock'").Scan(&service); err != nil || service != "rootless-docker" {
						t.Fatal(service, err)
					}
					if err := db.SQL.QueryRow(`SELECT json_extract(config,'$.dram.backend') FROM node_service_settings WHERE name='dram-bw'`).Scan(&backend); err != nil || backend != "mock" {
						t.Fatal(backend, err)
					}
					var owner, fingerprint string
					if err := db.SQL.QueryRow("SELECT owner,fingerprint FROM managed_containers WHERE id='kept-container'").Scan(&owner, &fingerprint); err != nil || owner != "alice" || fingerprint != "kept-fingerprint" {
						t.Fatal("container changed", owner, fingerprint, err)
					}
					if _, err := db.SQL.Exec(`INSERT INTO node_service_mounts(container_id,endpoint,daemon,owner,name,socket_path) VALUES('kept-container','unix:///test.sock','daemon','alice','kept-name','/var/run/docker.sock')`); err != nil {
						t.Fatal(err)
					}
					if _, err := db.SQL.Exec("UPDATE managed_containers SET owner='bob' WHERE id='kept-container'"); err == nil {
						t.Fatal("missing ownership protection")
					}
				} else if tables != 0 {
					t.Fatal("worker-only schema installed on", role)
				}
				var identity, password string
				if err := db.SQL.QueryRow("SELECT instance_id FROM service_identity WHERE id=1").Scan(&identity); err != nil || identity != "persistent-instance" {
					t.Fatal(identity, err)
				}
				if err := db.SQL.QueryRow("SELECT password_hash FROM users WHERE id='user'").Scan(&password); err != nil || password != "existing-password-hash" {
					t.Fatal(password, err)
				}
			})
		}
	}
}

func TestDRAMMountMigrationRollback(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		t.Run(entry, func(t *testing.T) {
			db := dramMountOldDatabase(t, "worker")
			if _, err := db.SQL.Exec(`CREATE TRIGGER fail_dram_upgrade BEFORE UPDATE ON node_service_settings BEGIN SELECT RAISE(ABORT,'injected DRAM upgrade failure'); END;`); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil {
				t.Fatal("upgrade failure ignored")
			}
			assertVersion(t, db, 41)
			var count int
			if err := db.SQL.QueryRow("SELECT count(*) FROM pragma_table_info('node_service_mounts') WHERE name='service'").Scan(&count); err != nil || count != 0 {
				t.Fatal("partial migration survived", count, err)
			}
			var socket string
			if err := db.SQL.QueryRow("SELECT socket_path FROM node_service_mounts").Scan(&socket); err != nil || socket != "/run/kept.sock" {
				t.Fatal("mount lost", socket, err)
			}
		})
	}
}
