package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestSnapshotViewProjectionAndInvalidation(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	id := strings.Repeat("e", 32)
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','test',1,2,?)", id, httpapi.JSONText(defaultConfig())); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.JobID = id
	// Large retained subtrees must not be transmitted when opening the overview.
	branch := &Node{Name: "bulk", Path: "/bulk", Kind: "directory", Children: []*Node{}}
	for i := range 30000 {
		branch.Children = append(branch.Children, &Node{Name: fmt.Sprintf("file-%d", i), Path: fmt.Sprintf("/bulk/file-%d", i), Kind: "file", Allocated: 4096, Apparent: 2048, Children: []*Node{}})
	}
	branch.Allocated = int64(len(branch.Children)) * 4096
	branch.Apparent = int64(len(branch.Children)) * 2048
	snapshot.Tree.Children = append(snapshot.Tree.Children, branch)
	snapshot.Tree.Allocated += branch.Allocated
	snapshot.Tree.Apparent += branch.Apparent
	file := filepath.Join(p.db.Directory, "results", id, "snapshot.json")
	if err = atomicWrite(file, &snapshot); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/jobs/" + id + "/view"
	first := p.Expect(200, "GET", endpoint, nil, nil)
	encoded := httpapi.JSONText(first)
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > len(raw)/20 || strings.Contains(encoded, "file-29999") {
		t.Fatalf("view contains unopened descendants: %d vs %d", len(encoded), len(raw))
	}
	t.Logf("overview %d bytes; full snapshot %d bytes", len(encoded), len(raw))
	expected := buildUsage(&snapshot)
	stats := first["usage"].(map[string]any)
	if numberInt64(stats["exclusive"]) != expected.Exclusive || numberInt64(stats["unrelated"]) != expected.Unrelated {
		t.Fatal("projection changed accounting")
	}
	cached := p.api.views.view
	p.Expect(200, "GET", endpoint+"?path=%2Fbulk", nil, nil)
	if p.api.views.view != cached {
		t.Fatal("directory navigation rebuilt immutable accounting")
	}
	// Changing an owner without a directory revision must invalidate statistics.
	ownerID := snapshot.Containers[0].ID
	p.Expect(200, "PUT", "/api/owners", object{"container_id": ownerID, "owner": "new-owner"}, nil)
	changed := p.Expect(200, "GET", endpoint, nil, nil)
	if p.api.views.view == cached || !strings.Contains(httpapi.JSONText(changed), "new-owner") {
		t.Fatal("owner edit reused stale accounting")
	}
	snapshot.Revision++
	if err = atomicWrite(file, &snapshot); err != nil {
		t.Fatal(err)
	}
	changed = p.Expect(200, "GET", endpoint, nil, nil)
	if numberInt64(changed["metadata"].(map[string]any)["revision"]) != snapshot.Revision {
		t.Fatal("baseline revision was cached")
	}
	for _, query := range []string{"?path=relative", "?path=/a/../b", "?path=/a&path=/b", "?unknown=1"} {
		p.Expect(400, "GET", endpoint+query, nil, nil)
	}
	p.Expect(401, "GET", endpoint, nil, map[string]string{"Cookie": ""})
	p.Expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
	p.Expect(404, "GET", endpoint, nil, nil)
}

func TestSnapshotViewReferencesAndHostAccounting(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	u := buildUsage(&snapshot)
	view := snapshotView(&snapshot, u, "")
	for _, n := range view["nodes"].([]object) {
		path := n["path"].(string)
		if n["partial"] != u.Incomplete[path] {
			t.Fatal("incomplete subtree lost")
		}
		host := n["host"].(object)
		if host["allocated"] != u.HostAllocated[path] {
			t.Fatal("host accounting lost")
		}
	}
	// A source alias includes its resolved target, but not its entire subtree.
	target := snapshot.Tree.Children[0]
	alias := &Node{Name: "alias", Path: "/alias", Kind: "reference", Reference: target.Path}
	middle := &Node{Name: "middle", Path: "/unopened/middle", Kind: "reference", Reference: target.Path}
	alias.Reference = middle.Path
	snapshot.Tree.Children = append(snapshot.Tree.Children, alias, &Node{Name: "unopened", Path: "/unopened", Kind: "directory", Children: []*Node{middle}})
	u = buildUsage(&snapshot)
	view = snapshotView(&snapshot, u, "/alias")
	found := false
	intermediate := false
	for _, n := range view["nodes"].([]object) {
		if n["path"] == middle.Path {
			intermediate = true
		}
		if n["path"] == target.Path {
			found = n["loaded"] == true
		}
	}
	if !found || !intermediate {
		t.Fatal("reference target was not opened")
	}
	// Cancelled view requests cannot publish a new cache entry.
	p := newTestPlatform(t)
	r := httptest.NewRequest("GET", "/", nil)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	if _, err = p.api.readSnapshotView(r.WithContext(ctx), strings.Repeat("a", 32)); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}

func TestSnapshotViewDatabaseRevisionNotification(t *testing.T) {
	p := newTestPlatform(t)
	p.Login(true, "administrator", "A-test-password-123")
	id := strings.Repeat("f", 32)
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?)", id, httpapi.JSONText(defaultConfig())); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.JobID = id
	snapshot.Revision = 1
	save := func(previous *Snapshot) {
		t.Helper()
		tx, err := p.db.SQL.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = storeSnapshot(tx, id, snapshot.Tree.Path, &snapshot, previous); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	save(nil)
	endpoint := "/api/jobs/" + id + "/view"
	p.Expect(200, "GET", endpoint, nil, nil)
	cached := p.api.views.view
	previous := snapshot
	snapshot.Revision = 2
	save(&previous)
	current := p.Expect(200, "GET", endpoint, nil, nil)
	if p.api.views.view == cached || numberInt64(current["metadata"].(map[string]any)["revision"]) != 2 {
		t.Fatal("database publication did not invalidate view")
	}
	r := httptest.NewRequest("GET", "/api/jobs/"+id+"/events?view=1&revision=0", nil)
	r.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	response := httptest.NewRecorder()
	_, _, err = p.api.streamSnapshot(response, r.WithContext(ctx), id)
	if err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	if !strings.Contains(body, "id: 2\nevent: changes\n") || strings.Contains(body, "tree") || strings.Contains(body, "replacements") || len(body) > 150 {
		t.Fatalf("notification shipped snapshot data: %s", body)
	}
}
