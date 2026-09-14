package storage

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestRecordQueriesShareWebExploration(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", nil, nil)
	id := job["id"].(string)
	if done := waitJob(t, p.db, id); done["status"] != "completed" {
		t.Fatalf("scan failed: %v", done)
	}
	if err := p.api.Service.Wait(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{"path": json.RawMessage(httpapi.JSONText(p.storage))}
	compare := func() object {
		t.Helper()
		shared, err := p.api.Service.Query(id, "directory", fields)
		if err != nil {
			t.Fatal(err)
		}
		web := p.expect(200, "GET", "/api/jobs/"+id+"/directory?path="+url.QueryEscape(p.storage), nil, nil)
		var normalized object
		if err := json.Unmarshal([]byte(httpapi.JSONText(shared)), &normalized); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalized, web) {
			t.Fatalf("web and service differ: %v / %v", web, shared)
		}
		return shared
	}
	initial := compare()
	mustWrite(t, filepath.Join(p.storage, "new.parquet"), make([]byte, 32768))
	expansion := p.expect(202, "POST", "/api/jobs/"+id+"/expand", object{"path": p.storage, "revision": initial["revision"], "depth": 2}, nil)
	if done := waitJob(t, p.db, expansion["id"].(string)); done["status"] != "completed" {
		t.Fatalf("expansion failed: %v", done)
	}
	current := compare()
	if numberInt64(current["revision"]) <= numberInt64(initial["revision"]) || !strings.Contains(httpapi.JSONText(current["analysis"]), "new.parquet") {
		t.Fatalf("stale query: %v", current)
	}
	p.expect(409, "POST", "/api/jobs/"+id+"/expand", object{"path": p.storage, "revision": initial["revision"], "depth": 1}, nil)
	snapshot, err := p.api.Service.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := p.api.Service.Changes(id, numberInt64(initial["revision"]))
	if err != nil {
		t.Fatal(err)
	}
	if numberInt64(snapshot["revision"]) != numberInt64(current["revision"]) || numberInt64(patch["revision"]) != numberInt64(current["revision"]) {
		t.Fatal("snapshot, query and patch revisions differ")
	}
	reopened, err := openDatabase(p.db.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.SQL.Close()
	saved, err := NewService(reopened, nil).Query(id, "directory", fields)
	if err != nil || httpapi.JSONText(saved) != httpapi.JSONText(current) {
		t.Fatalf("directory analysis was not durable: %v %v", saved, err)
	}
	for _, path := range []string{"/containers?sort_by=bad", "/container?container=missing", "/directory?path=relative", "/owners?limit=51", "/overview?unexpected=x", "/owners?offset=0&offset=1"} {
		status := 400
		if strings.HasPrefix(path, "/container?") {
			status = 404
		}
		p.expect(status, "GET", "/api/jobs/"+id+path, nil, nil)
	}
	p.expect(201, "POST", "/api/users", object{"username": "reader", "password": "Reader-password-123", "role": "viewer"}, nil)
	p.login(false, "reader", "Reader-password-123")
	compare()
	p.expect(403, "POST", "/api/jobs/"+id+"/expand", object{"path": p.storage, "revision": current["revision"]}, nil)
}

func TestDirectoryAnalysisInvalidation(t *testing.T) {
	base, c, target, other := incrementalFixture(t)
	observation := DirectoryAnalysis{ObservedAt: "old", Analysis: &FileAnalysis{}}
	base.DirectoryAnalyses = map[string]DirectoryAnalysis{target: observation, filepath.Join(target, "a"): observation, filepath.Dir(target): observation, other: observation}
	before := httpapi.JSONText(base.DirectoryAnalyses)
	published := 0
	result, err := expandDirectory(context.Background(), base, c, target, nil, func(partial *Snapshot) error {
		published++
		if len(partial.DirectoryAnalyses) != 1 || partial.DirectoryAnalyses[other].ObservedAt != "old" {
			t.Fatal("partial update retained overlapping file statistics")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if published == 0 {
		t.Fatal("missing incremental publications")
	}
	if httpapi.JSONText(base.DirectoryAnalyses) != before {
		t.Fatal("mutated old record's file statistics")
	}
	if len(result.DirectoryAnalyses) != 2 || result.DirectoryAnalyses[target].Analysis == nil || result.DirectoryAnalyses[target].ObservedAt != result.UpdatedAt || result.DirectoryAnalyses[other].ObservedAt != "old" {
		t.Fatalf("incorrect final statistics: %v", result.DirectoryAnalyses)
	}
}
