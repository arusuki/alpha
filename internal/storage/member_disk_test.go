package storage

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMemberDiskOwnershipAndDirectoryBoundary(t *testing.T) {
	p := newTestPlatform(t)
	user := platform.User{ID: strings.Repeat("1", 32), Username: "member:yuuka", Role: "viewer"}
	call := func(query string, want int) object {
		t.Helper()
		_, value, err := p.api.Dispatch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/member-disk"+query, nil), user)
		code := 200
		if err != nil {
			var api *httpapi.Error
			if !errors.As(err, &api) {
				t.Fatal(err)
			}
			code = api.Status
		}
		if code != want {
			t.Fatalf("%s: got %d, want %d: %v", query, code, want, err)
		}
		if err != nil {
			return nil
		}
		return value.(object)
	}
	call("", 404)
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	snapshot.JobID = id
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','test',1,2,?)", id, httpapi.JSONText(defaultConfig())); err != nil {
		t.Fatal(err)
	}
	mine, other := snapshot.Containers[0], snapshot.Containers[1]
	// A root bind is access only. An inode alias into another container must
	// neither reveal its target path nor grant directory navigation.
	snapshot.Resources = append(snapshot.Resources, Resource{Path: "/", Kinds: []string{"bind"}, Containers: []string{mine.ID}})
	upper := snapshotNodes(snapshot.Tree)[*mine.UpperPath]
	alias := *mine.UpperPath + "/alias"
	upper.Children = append(upper.Children, &Node{Name: "alias", Path: alias, Kind: "reference", Reference: *other.UpperPath})
	upper.Children = append(upper.Children, &Node{Name: "shared-alias", Path: *mine.UpperPath + "/shared-alias", Kind: "reference", Reference: "/srv/shared/models"})
	for i := range 60 {
		upper.Children = append(upper.Children, &Node{Name: "small", Path: *mine.UpperPath + "/small-" + strings.Repeat("x", i+1), Kind: "file", Allocated: 4096})
		upper.Allocated += 4096
	}
	snapshotNodes(snapshot.Tree)["/var/lib/docker"].Allocated += 60 * 4096
	snapshot.Tree.Allocated += 60 * 4096
	if err := atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), &snapshot); err != nil {
		t.Fatal(err)
	}
	overview := call("", 200)
	if strings.Contains(httpapi.JSONText(overview), *other.UpperPath) || strings.Contains(httpapi.JSONText(overview), "DeepSeek-R1") {
		t.Fatal("overview exposed source paths or descendants")
	}
	for _, row := range overview["containers"].([]object) {
		if row["expandable"] != (row["id"] == mine.ID) {
			t.Fatal("incorrect ownership", row)
		}
	}
	want := buildUsage(&snapshot)
	if overview["exclusive"] != want.Exclusive || overview["shared"] != want.Shared {
		t.Fatal("member filtering changed disk accounting")
	}
	base := "?container=" + mine.ID
	detail := call(base, 200)
	if detail["shared"] != want.Containers[mine.ID].Shared {
		t.Fatal("shared bytes were split")
	}
	if strings.Contains(httpapi.JSONText(detail), *other.UpperPath) {
		t.Fatal("foreign source leaked")
	}
	for _, source := range detail["sources"].([]object) {
		if source["path"] == "/" {
			t.Fatal("root bind became browsable")
		}
	}
	directory := call(base+"&path="+url.QueryEscape(*mine.UpperPath), 200)
	if len(directory["entries"].([]object)) != 50 || directory["has_more"] != true {
		t.Fatal("directory pagination missing")
	}
	if strings.Contains(httpapi.JSONText(directory), *other.UpperPath) {
		t.Fatal("reference target leaked")
	}
	secondPage := call(base+"&path="+url.QueryEscape(*mine.UpperPath)+"&offset=50", 200)
	for _, page := range []object{directory, secondPage} {
		total := page["self_and_omitted_allocated"].(int64) + page["other_entries_allocated"].(int64)
		if page["other_entries_allocated"].(int64) <= 0 {
			t.Fatal("off-page bytes missing from capacity map", page)
		}
		for _, entry := range page["entries"].([]object) {
			total += entry["allocated"].(int64)
			if entry["name"] == "shared-alias" && (entry["allocated"] != int64(0) || entry["expandable"] != true) {
				t.Fatal("reference duplicated target bytes or lost navigation", entry)
			}
		}
		if total != upper.Allocated {
			t.Fatal("capacity map does not account for the entire directory", total, upper.Allocated)
		}
	}
	call(base+"&path="+url.QueryEscape("/srv/shared/models"), 200)
	call("?container="+other.ID, 403)
	call("?container=missing", 403)
	for _, path := range []string{"/", "/etc", *other.UpperPath, alias, *mine.UpperPath + "/../diff", *mine.UpperPath + "-sibling"} {
		call(base+"&path="+url.QueryEscape(path), 403)
	}
	for _, query := range []string{"?owner=lin", "?container=", base + "&container=" + other.ID, "?path=/", base + "&offset=0", base + "&path=/&offset=-1", "?%ZZ=1"} {
		call(query, 400)
	}
	// ReadSnapshot must apply the latest owner override on every detail request.
	if err := p.db.setOwner(mine.ID, "lin", "test"); err != nil {
		t.Fatal(err)
	}
	call(base, 403)
	user.Username = "administrator"
	call("", 403)
}
