package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func waitChangesWorker(t *testing.T, p *testPlatform, id string) {
	t.Helper()
	if result := waitJob(t, p.db, id); result["status"] != "completed" {
		t.Fatal(result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.m.mu.Lock()
		err := p.m.reapLocked()
		idle := p.m.process == nil
		p.m.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if idle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker did not exit")
}

// Applying only the wire response to a previously loaded snapshot must produce
// the same public result as a full download, including removed optional fields.
func assertChangesReconstruct(t *testing.T, previous, changes, current object) {
	t.Helper()
	var before object
	if err := json.Unmarshal([]byte(httpapi.JSONText(previous)), &before); err != nil {
		t.Fatal(err)
	}
	replacements := map[string]map[string]any{}
	for _, value := range changes["replacements"].([]any) {
		n := value.(map[string]any)
		replacements[n["path"].(string)] = n
	}
	ancestors := map[string]map[string]any{}
	for _, value := range changes["ancestors"].([]any) {
		n := value.(map[string]any)
		if _, ok := n["children"]; ok {
			t.Fatal("ancestor leaked a subtree")
		}
		ancestors[n["path"].(string)] = n
	}
	var apply func(map[string]any) map[string]any
	apply = func(n map[string]any) map[string]any {
		path := n["path"].(string)
		if replacement := replacements[path]; replacement != nil {
			return replacement
		}
		children, _ := n["children"].([]any)
		for i, child := range children {
			children[i] = apply(child.(map[string]any))
		}
		if fields := ancestors[path]; fields != nil {
			n = map[string]any{}
			for key, value := range fields {
				n[key] = value
			}
			n["children"] = children
		}
		return n
	}
	rebuilt := object{}
	for key, value := range changes["metadata"].(map[string]any) {
		rebuilt[key] = value
	}
	rebuilt["tree"] = apply(before["tree"].(map[string]any))
	if !reflect.DeepEqual(rebuilt, current) {
		t.Fatalf("incremental response did not reconstruct current snapshot\nrebuilt: %s\ncurrent: %s", httpapi.JSONText(rebuilt), httpapi.JSONText(current))
	}
}

func TestSnapshotChangesWorkerExpansionAndSkippedRevisions(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	c := p.configure()
	target := filepath.Join(p.storage, "target")
	deeper := filepath.Join(target, "deeper")
	other := filepath.Join(p.storage, "other")
	unrelated := filepath.Join(p.storage, "untouched")
	for _, dir := range []string{deeper, other, filepath.Join(unrelated, "hidden")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "data.bin"), make([]byte, 8192))
	}
	c.MaxDepth = 1
	p.Expect(200, "PUT", "/api/settings", object{"revision": 2, "value": c}, nil)
	id := p.Expect(202, "POST", "/api/jobs", nil, nil)["id"].(string)
	waitChangesWorker(t, p, id)
	endpoint := "/api/jobs/" + id
	baseline := p.Expect(200, "GET", endpoint+"/snapshot", nil, nil)
	unchanged := p.Expect(200, "GET", endpoint+"/changes?revision=0", nil, nil)
	assertChangesReconstruct(t, baseline, unchanged, baseline)
	if len(unchanged["replacements"].([]any)) != 0 || len(unchanged["ancestors"].([]any)) != 0 {
		t.Fatal("unchanged revision returned tree data")
	}
	var first object
	var from, firstRevision int64
	for revision, path := range []string{target, deeper, other} {
		mustWrite(t, filepath.Join(path, "new.bin"), make([]byte, 16384*(revision+1)))
		job := p.Expect(202, "POST", endpoint+"/expand", object{"path": path, "revision": from, "depth": 1}, nil)
		waitChangesWorker(t, p, job["id"].(string))
		updated := p.Expect(200, "GET", endpoint+"/snapshot", nil, nil)
		from = numberInt64(updated["revision"])
		if revision == 0 {
			firstRevision = from
			first = p.Expect(200, "GET", endpoint+"/snapshot", nil, nil)
			changes := p.Expect(200, "GET", endpoint+"/changes?revision=0", nil, nil)
			if changes["job_id"] != id || changes["base_revision"] != float64(0) || numberInt64(changes["revision"]) != from {
				t.Fatal("wrong change response identity or revision", changes)
			}
			rows := changes["replacements"].([]any)
			if len(rows) != 1 || rows[0].(map[string]any)["path"] != target {
				t.Fatal("first expansion returned unrelated replacements")
			}
			if strings.Contains(httpapi.JSONText(changes["replacements"]), unrelated) || strings.Contains(httpapi.JSONText(changes["ancestors"]), unrelated) {
				t.Fatal("unrelated directory tree leaked in incremental payload")
			}
			assertChangesReconstruct(t, baseline, changes, first)
		}
	}
	current := p.Expect(200, "GET", endpoint+"/snapshot", nil, nil)
	for index, previous := range []object{baseline, first} {
		revision := []int64{0, firstRevision}[index]
		changes := p.Expect(200, "GET", endpoint+fmt.Sprintf("/changes?revision=%d", revision), nil, nil)
		if len(changes["replacements"].([]any)) != 2 {
			t.Fatal("overlapping changed paths were not coalesced", changes)
		}
		if strings.Contains(httpapi.JSONText(changes), "incremental_accounting") || strings.Contains(httpapi.JSONText(changes["replacements"]), unrelated) {
			t.Fatal("incremental payload leaked private data or an unrelated subtree")
		}
		assertChangesReconstruct(t, previous, changes, current)
	}
	unchanged = p.Expect(200, "GET", endpoint+fmt.Sprintf("/changes?revision=%d", from), nil, nil)
	assertChangesReconstruct(t, current, unchanged, current)
	p.Expect(201, "POST", "/api/users", object{"username": "reader", "password": "A-reader-password-123", "role": "viewer"}, nil)
	p.Login(false, "reader", "A-reader-password-123")
	p.Expect(200, "GET", endpoint+"/changes?revision=0", nil, nil)
	p.Expect(403, "POST", endpoint+"/expand", object{"path": target, "revision": 3}, nil)
	p.Expect(401, "GET", endpoint+"/changes?revision=0", nil, map[string]string{"Cookie": ""})
}

func TestSnapshotChangesPublicMetadataAndRevisionValidation(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	id := platform.RandomHex(16)
	target := filepath.Join(p.storage, "target")
	base := &Snapshot{
		SchemaVersion: snapshotVersion,
		Tree:          &Node{Name: "root", Path: "@root", Kind: "root", Children: []*Node{{Name: "target", Path: target, Kind: "directory", Children: []*Node{}}}},
		Containers:    []Container{{ID: "fixture-container", Owner: "label-owner"}},
		Accounting:    []InodeRecord{{Path: "/private-ledger"}},
	}
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,'{}')", id); err != nil {
		t.Fatal(err)
	}
	base.Revision = 1
	if err := p.db.Transaction(func(tx *sql.Tx) error {
		if err := storeSnapshot(tx, id, target, base, nil); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO snapshot_changes(job_id,revision,path) VALUES(?,1,?)", id, target)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.SQL.Exec("INSERT INTO owners(container_id,owner) VALUES('fixture-container','platform-owner')"); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/jobs/" + id + "/changes"
	changes := p.Expect(200, "GET", endpoint+"?revision=0", nil, nil)
	metadata := changes["metadata"].(map[string]any)
	container := metadata["containers"].([]any)[0].(map[string]any)
	if container["owner"] != "platform-owner" || container["label_owner"] != "label-owner" {
		t.Fatal("changes bypassed current owner overlay", container)
	}
	if _, ok := metadata["tree"]; ok {
		t.Fatal("metadata included the full tree")
	}
	if strings.Contains(httpapi.JSONText(changes), "incremental_accounting") || strings.Contains(httpapi.JSONText(changes), "/private-ledger") {
		t.Fatal("private accounting leaked")
	}
	for _, suffix := range []string{"", "?revision=", "?revision=-1", "?revision=2", "?revision=1.0", "?revision=oops", "?revision=0&revision=1", "?revision=9223372036854775808"} {
		p.Expect(400, "GET", endpoint+suffix, nil, nil)
	}
	p.Expect(404, "GET", "/api/jobs/"+platform.RandomHex(16)+"/changes?revision=0", nil, nil)
	p.Expect(200, "GET", endpoint+"?revision=1", nil, nil)
}
