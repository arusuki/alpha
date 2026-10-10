package updater

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func nodeServicesOldDatabase(t *testing.T, role string, version int) *platform.Database {
	t.Helper()
	base := version
	if base > 38 {
		base = 38
	}
	db := scheduledAnalysisOldDatabase(t, role, base)
	if version >= 39 {
		var query string
		switch role {
		case "worker":
			query = `ALTER TABLE settings ADD COLUMN analysis_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_status TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_error TEXT NOT NULL DEFAULT '';
UPDATE settings SET value=json_set(value,'$.auto_agent_analyze',json('false')),revision=revision+1;
UPDATE jobs SET config=json_set(config,'$.auto_agent_analyze',json('false'));`
		case "control":
			query = `ALTER TABLE agent_sessions ADD COLUMN scheduled_job_id TEXT;
CREATE UNIQUE INDEX idx_agent_scheduled ON agent_sessions(node_id,scheduled_job_id,report_scope) WHERE scheduled_job_id IS NOT NULL;`
		}
		if _, err := db.SQL.Exec(query + "PRAGMA user_version=39;"); err != nil {
			t.Fatal(err)
		}
	}
	if role == "worker" {
		if _, err := db.SQL.Exec(`INSERT INTO managed_containers VALUES('kept-container','unix:///test.sock','daemon','kept-name','alice','{}','kept-fingerprint','adopt','',1,123)`); err != nil {
			t.Fatal(err)
		}
	}
	if version == 40 {
		if role == "worker" {
			if err := db.Transaction(platform.InstallNodeServices); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL.Exec(`DROP TABLE node_service_settings;
CREATE TABLE node_service_settings(name TEXT PRIMARY KEY, config TEXT NOT NULL CHECK(json_valid(config)));
INSERT INTO node_service_settings VALUES('dram-bw','{"restart_policy":"on-failure","stop_timeout":35,"control_socket":"","socket_path":"","users":[]}');
INSERT INTO node_service_settings VALUES('rootless-docker','{"restart_policy":"always","stop_timeout":150,"control_socket":"/run/custom/control.sock","socket_path":"/run/custom/docker.sock","users":["alice"]}');`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.SQL.Exec("PRAGMA user_version=40"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func TestNodeServicesBuiltUpdater(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"worker", "control", "registry"} {
			for _, version := range []int{37, 38, 39, 40} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := nodeServicesOldDatabase(t, role, version)
					for range 2 {
						if err := runScheduledMigration(t, binary, entry, role, db); err != nil {
							t.Fatal(err)
						}
						assertVersion(t, db, platform.DatabaseVersion)
					}
					var tables int
					if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('node_service_settings','node_service_mounts')").Scan(&tables); err != nil {
						t.Fatal(err)
					}
					if role == "worker" {
						if tables != 2 {
							t.Fatal("missing service tables", tables)
						}
						if version == 40 {
							var backend, users, socket, policy string
							var interval, timeout int
							if err := db.SQL.QueryRow(`SELECT json_extract(config,'$.dram.backend'),json_extract(config,'$.dram.interval_us'),json_extract(config,'$.stop_timeout'),json_extract(config,'$.restart_policy') FROM node_service_settings WHERE name='dram-bw'`).Scan(&backend, &interval, &timeout, &policy); err != nil || backend != "amd-rome" || interval != 100000 || timeout != 35 || policy != "on-failure" {
								t.Fatal(backend, interval, timeout, policy, err)
							}
							if err := db.SQL.QueryRow(`SELECT json_extract(config,'$.users'),json_extract(config,'$.socket_path') FROM node_service_settings WHERE name='rootless-docker'`).Scan(&users, &socket); err != nil || users != `["alice"]` || socket != "/run/custom/docker.sock" {
								t.Fatal(users, socket, err)
							}
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
		t.Run(entry+"/rollback", func(t *testing.T) {
			db := nodeServicesOldDatabase(t, "worker", 39)
			if _, err := db.SQL.Exec("CREATE TABLE node_service_mounts(sentinel TEXT); INSERT INTO node_service_mounts VALUES('preserved')"); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil {
				t.Fatal("conflicting schema accepted")
			}
			assertVersion(t, db, 39)
			var count int
			var sentinel string
			if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='node_service_settings'").Scan(&count); err != nil || count != 0 {
				t.Fatal("partial schema survived", count, err)
			}
			if err := db.SQL.QueryRow("SELECT sentinel FROM node_service_mounts").Scan(&sentinel); err != nil || sentinel != "preserved" {
				t.Fatal("existing data changed", sentinel, err)
			}
		})
		t.Run(entry+"/parameters-rollback", func(t *testing.T) {
			db := nodeServicesOldDatabase(t, "worker", 40)
			if _, err := db.SQL.Exec(`UPDATE node_service_settings SET config='[]' WHERE name='dram-bw'`); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil {
				t.Fatal("malformed configuration accepted")
			}
			assertVersion(t, db, 40)
			var raw string
			if err := db.SQL.QueryRow("SELECT config FROM node_service_settings WHERE name='dram-bw'").Scan(&raw); err != nil || raw != "[]" {
				t.Fatal("original config lost", raw, err)
			}
			var count int
			if err := db.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='node_service_settings_v40'").Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
		t.Run(entry+"/unsupported", func(t *testing.T) {
			db := nodeServicesOldDatabase(t, "worker", 37)
			if _, err := db.SQL.Exec("PRAGMA user_version=36"); err != nil {
				t.Fatal(err)
			}
			if err := runScheduledMigration(t, binary, entry, "worker", db); err == nil || !strings.Contains(err.Error(), "unsupported database version") {
				t.Fatal("unsupported accepted", err)
			}
			assertVersion(t, db, 36)
			var name string
			if err := db.SQL.QueryRow("SELECT name FROM managed_containers WHERE id='kept-container'").Scan(&name); err != nil || name != "kept-name" {
				t.Fatal("unsupported data changed", name, err)
			}
		})
	}
}
