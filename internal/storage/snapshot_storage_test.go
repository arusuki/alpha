package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestDirectoryStorageReusesWorkerAndUpdatesOnlyChangedNodes(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	_, c, target, unrelated := incrementalFixture(t)
	// One snapshot contains both a container path and an independent host path.
	host := filepath.Join(c.Root[0], "host-data")
	if err := os.MkdirAll(filepath.Join(host, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(host, "nested", "file"), make([]byte, 12288))
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Containers = []Container{{ID: "fixture-container", Name: "fixture", UpperPath: &target, Mounts: []ContainerMount{}}}
	id := platform.RandomHex(16)
	base.JobID = id
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?)", id, httpapi.JSONText(c)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(p.db.Directory, "results", id, "snapshot.json")
	if err := atomicWrite(file, base); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(file)
	var worker string
	var revision int64
	for i, path := range []string{target, host, filepath.Join(host, "nested"), target} {
		if i == 3 {
			if err := os.Remove(filepath.Join(target, "live.bin")); err != nil {
				t.Fatal(err)
			}
		}
		previous := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
		job := p.expect(202, "POST", "/api/jobs/"+id+"/expand", object{"path": path, "revision": revision}, nil)
		if i == 0 {
			worker = job["id"].(string)
		} else if job["id"] != worker {
			t.Fatal("each click created a new worker record")
		}
		waitChangesWorker(t, p, worker)
		current := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
		patch := p.expect(200, "GET", fmt.Sprintf("/api/jobs/%s/changes?revision=%d", id, revision), nil, nil)
		assertChangesReconstruct(t, previous, patch, current)
		revision = numberInt64(current["revision"])
		if i == 0 {
			// Subsequent scans must leave unrelated persisted nodes untouched.
			if _, err := p.db.SQL.Exec(`CREATE TRIGGER protect_unrelated BEFORE UPDATE ON snapshot_nodes
 WHEN OLD.path = ` + "'" + strings.ReplaceAll(unrelated, "'", "''") + "'" + ` BEGIN SELECT RAISE(ABORT,'rewrote unrelated node'); END`); err != nil {
				t.Fatal(err)
			}
		}
		state := p.expect(200, "GET", "/api/state", nil, nil)
		if len(state["jobs"].([]any)) != 1 || len(state["directory_jobs"].([]any)) != 1 {
			t.Fatal("directory clicks accumulated scan history", state)
		}
		files, _ := filepath.Glob(filepath.Join(p.db.Directory, "results", worker, "snapshot*.json"))
		if len(files) != 0 {
			t.Fatal("directory updates copied full snapshots", files)
		}
	}
	saved, err := p.db.readSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotNodes(saved.Tree)[filepath.Join(target, "live.bin")] != nil {
		t.Fatal("deleted directory entry remained in current storage")
	}
	var count int
	if err := p.db.SQL.QueryRow("SELECT count(*) FROM snapshot_nodes WHERE job_id=?", id).Scan(&count); err != nil || count != len(snapshotNodes(saved.Tree)) {
		t.Fatalf("old node versions accumulated: %d %v", count, err)
	}
	if err := p.db.SQL.QueryRow("SELECT count(*) FROM snapshot_changes WHERE job_id=?", id).Scan(&count); err != nil || count != 3 {
		t.Fatalf("repeated directory updates accumulated change history: %d %v", count, err)
	}
	if err := p.db.SQL.QueryRow("SELECT json_type(metadata,'$.incremental_accounting') IS NOT NULL FROM snapshot_records WHERE job_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("metadata duplicates the node identity ledger: %d %v", count, err)
	}
	after, _ := os.ReadFile(file)
	if string(original) != string(after) {
		t.Fatal("directory scanning rewrote baseline")
	}
	// Reopening a separate database connection reconstructs the committed data.
	reopened, err := openDatabase(p.db.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.SQL.Close()
	read, err := reopened.readSnapshot(id)
	if err != nil || httpapi.JSONText(read) != httpapi.JSONText(saved) {
		t.Fatalf("reopening lost accumulated details: %v", err)
	}
	if _, err := p.m.Delete(id, "test"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"snapshot_records", "snapshot_nodes", "snapshot_changes"} {
		if err := p.db.SQL.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("deleting record left %s data: %d %v", table, count, err)
		}
	}
}

func TestDirectoryProcessCleanupPreservesPublishedResults(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelled), func(t *testing.T) {
			p := newTestPlatform(t)
			base, c, target, _ := incrementalFixture(t)
			id := platform.RandomHex(16)
			if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?)", id, httpapi.JSONText(c)); err != nil {
				t.Fatal(err)
			}
			if err := atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), base); err != nil {
				t.Fatal(err)
			}
			originalCommand := p.m.command
			p.m.command = func(string, string) *exec.Cmd { return exec.Command("sleep", "60") }
			job, err := p.m.StartIncremental("test", id, target, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			worker := job["id"].(string)
			if _, err := p.db.SQL.Exec("UPDATE jobs SET status='running' WHERE id=?", worker); err != nil {
				t.Fatal(err)
			}
			plan := scanPlan{Config: c, BaseJobID: id, IncrementalPath: target}
			partial, err := expandForTest(context.Background(), base, c, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.db.publishDirectory(context.Background(), worker, plan, partial, object{}, false, nil); err != nil {
				t.Fatal(err)
			}
			if cancelled {
				if _, err := p.m.Cancel(worker, "test"); err != nil {
					t.Fatal(err)
				}
			} else {
				p.m.mu.Lock()
				p.m.process.Process.Kill()
				if err := p.m.finishLocked(<-p.m.done); err != nil {
					t.Fatal(err)
				}
				p.m.mu.Unlock()
			}
			stored, err := p.db.readSnapshot(id)
			if err != nil || stored.Revision != 1 || len(snapshotNodes(stored.Tree)[target].Children) == 0 {
				t.Fatalf("process cleanup destroyed committed details: %v", err)
			}
			if _, err := p.api.Changes(id, 0); err != nil {
				t.Fatal(err)
			}
			p.m.command = originalCommand
			next, err := p.m.StartIncremental("test", id, target, stored.Revision, 1)
			if err != nil {
				t.Fatal("cannot continue after cleanup", err)
			}
			waitChangesWorker(t, p, next["id"].(string))
			if _, err := p.db.readSnapshot(id); err != nil {
				t.Fatal("next directory scan broke saved record", err)
			}
		})
	}
}

func TestDirectoryPublicationRollbackKeepsPreviousNodes(t *testing.T) {
	p := newTestPlatform(t)
	base, c, target, _ := incrementalFixture(t)
	id, worker := platform.RandomHex(16), platform.RandomHex(16)
	plan := scanPlan{Config: c, BaseJobID: id, IncrementalPath: target}
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?),(?,'running','incremental','test',2,?)", id, httpapi.JSONText(c), worker, httpapi.JSONText(plan)); err != nil {
		t.Fatal(err)
	}
	result, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.db.publishDirectory(context.Background(), worker, plan, result, object{}, false, nil); err != nil {
		t.Fatal(err)
	}
	previous, _ := p.db.readSnapshot(id)
	if _, err := p.db.SQL.Exec("CREATE TRIGGER reject_revision BEFORE INSERT ON snapshot_changes BEGIN SELECT RAISE(ABORT,'test publication rollback'); END"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(target, "new.bin"), make([]byte, 32768))
	result, err = expandForTest(context.Background(), previous, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	result.UpdatedAt = time.Now().String()
	if err := p.db.publishDirectory(context.Background(), worker, plan, result, object{}, false, previous); err == nil {
		t.Fatal("failed transaction was published")
	}
	stored, err := p.db.readSnapshot(id)
	if err != nil || httpapi.JSONText(stored) != httpapi.JSONText(previous) {
		t.Fatalf("failed publication modified saved nodes: %v", err)
	}
}

func TestDirectoryPublicationSkipsUnchangedRows(t *testing.T) {
	p := newTestPlatform(t)
	base, c, target, _ := incrementalFixture(t)
	c.MaxDepth = 8
	id, worker := platform.RandomHex(16), platform.RandomHex(16)
	plan := scanPlan{Config: c, BaseJobID: id, IncrementalPath: target}
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?),(?,'running','incremental','test',2,?)", id, httpapi.JSONText(c), worker, httpapi.JSONText(plan)); err != nil {
		t.Fatal(err)
	}
	first, err := expandDirectory(context.Background(), base, c, target, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.db.publishDirectory(context.Background(), worker, plan, first, object{}, false, nil); err != nil {
		t.Fatal(err)
	}
	unchanged := filepath.Join(target, "a", "b", "c", "d", "e", "file.bin")
	// BEFORE INSERT fires even for an UPSERT whose WHERE skips the update.
	// This proves unchanged nodes avoid executing the statement altogether.
	if _, err := p.db.SQL.Exec(`CREATE TRIGGER reject_unchanged_insert BEFORE INSERT ON snapshot_nodes
 WHEN NEW.path='` + strings.ReplaceAll(unchanged, "'", "''") + `' BEGIN SELECT RAISE(ABORT,'executed SQL for unchanged node'); END`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, "live.bin")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(target, "new.bin"), make([]byte, 32768))
	second, err := expandDirectory(context.Background(), first, c, target, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotNodes(first.Tree)[unchanged] == snapshotNodes(second.Tree)[unchanged] {
		t.Fatal("fixture must rescan into new node objects")
	}
	if err := p.db.publishDirectory(context.Background(), worker, plan, second, object{}, false, first); err != nil {
		t.Fatal(err)
	}
	stored, err := p.db.readSnapshot(id)
	if err != nil || httpapi.JSONText(stored.Tree) != httpapi.JSONText(second.Tree) {
		t.Fatalf("delta write lost updated counters, deletion or child positions: %v", err)
	}
	if len(stored.Accounting) != len(second.Accounting) {
		t.Fatal("delta write lost inode identities")
	}
	if err := p.db.validateIncrementalRequest(id, target, second.Revision); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unchanged, target + "/missing", "relative", target + "/../other"} {
		if err := p.db.validateIncrementalRequest(id, path, second.Revision); err == nil {
			t.Fatalf("admission accepted invalid target: %s", path)
		}
	}
	if err := p.db.validateIncrementalRequest(id, target, first.Revision); err == nil {
		t.Fatal("admission accepted stale revision")
	}
}
