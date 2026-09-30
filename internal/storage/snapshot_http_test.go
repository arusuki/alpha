package storage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestSnapshotConditionalDownload(t *testing.T) {
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
	file := filepath.Join(p.db.Directory, "results", id, "snapshot.json")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, file, raw)
	endpoint := "/api/jobs/" + id + "/snapshot"
	status, _, first := p.Request("GET", endpoint, nil, nil)
	etag := first.Header().Get("ETag")
	if status != 200 || etag != fmt.Sprintf(`"%x"`, sha256.Sum256(first.Body.Bytes())) {
		t.Fatalf("incorrect response hash: %d %s", status, etag)
	}
	for _, route := range []string{endpoint, "/api/snapshot"} {
		for _, match := range []string{etag, "W/" + etag, `"other", ` + etag} {
			status, _, response := p.Request("GET", route, nil, map[string]string{"If-None-Match": match})
			if status != 304 || response.Body.Len() != 0 || response.Header().Get("ETag") != etag {
				t.Fatalf("matching snapshot downloaded again: %d %s", status, response.Body)
			}
		}
	}
	p.Expect(401, "GET", endpoint, nil, map[string]string{"Cookie": "", "If-None-Match": etag})
	p.Expect(200, "PUT", "/api/owners", object{"container_id": strings.Repeat("a", 64), "owner": "changed"}, nil)
	status, data, changed := p.Request("GET", endpoint, nil, map[string]string{"If-None-Match": etag})
	if status != 200 || changed.Header().Get("ETag") == etag || data["containers"].([]any)[0].(map[string]any)["owner"] != "changed" {
		t.Fatal("owner override did not invalidate the cached result")
	}
	etag = changed.Header().Get("ETag")
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Revision++
	snapshot.Tree.Allocated++
	if err := atomicWrite(file, &snapshot); err != nil {
		t.Fatal(err)
	}
	status, _, changed = p.Request("GET", endpoint, nil, map[string]string{"If-None-Match": etag})
	if status != 200 || changed.Header().Get("ETag") == etag {
		t.Fatal("directory revision did not invalidate the cached result")
	}
	p.Expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
	p.Expect(404, "GET", endpoint, nil, map[string]string{"If-None-Match": changed.Header().Get("ETag")})
}
