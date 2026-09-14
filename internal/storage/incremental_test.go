package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func expandForTest(ctx context.Context, base *Snapshot, c Config, path string, progress func(object) error) (*Snapshot, error) {
	c.MaxDepth = incrementalDepth
	return expandDirectory(ctx, base, c, path, progress, nil)
}

func incrementalFixture(t *testing.T) (*Snapshot, Config, string, string) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(filepath.Join(target, "a", "b", "c", "d", "e"), 0700); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(other, 0700)
	mustWrite(t, filepath.Join(target, "a", "b", "c", "d", "e", "file.bin"), make([]byte, 16384))
	mustWrite(t, filepath.Join(other, "unrelated.bin"), make([]byte, 8192))
	mustWrite(t, filepath.Join(target, "live.bin"), make([]byte, 8192))
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{root}
	c.MaxDepth = 1
	c.MaxNodes = 100
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two containers share a physical source: frontend ownership is rebuilt from
	// the tree, and writable-layer summaries must match the same updated bytes.
	base.Containers = []Container{{ID: strings.Repeat("a", 64), Name: "first", UpperPath: &target, Mounts: []ContainerMount{}}, {ID: strings.Repeat("b", 64), Name: "second", UpperPath: &target, Mounts: []ContainerMount{}}}
	base.Resources = append(base.Resources, Resource{Path: target, Kinds: []string{"writable"}, Containers: []string{base.Containers[0].ID, base.Containers[1].ID}})
	return base, c, target, other
}
func assertSnapshotAccounting(t *testing.T, s *Snapshot) {
	t.Helper()
	if err := validateIncrementalNodes(snapshotNodes(s.Tree)); err != nil {
		t.Fatal(err)
	}
	var scanned int64
	for _, fs := range s.Filesystems {
		scanned += numberInt64(fs["scanned"])
		if numberInt64(fs["unexplained"]) != numberInt64(fs["used"])-numberInt64(fs["scanned"]) {
			t.Fatal("filesystem reconciliation differs")
		}
	}
	if scanned != s.Tree.Allocated {
		t.Fatalf("devices %d != tree %d", scanned, s.Tree.Allocated)
	}
}
func TestIncrementalGrowthShrinkAndRepeatedDetail(t *testing.T) {
	base, c, target, other := incrementalFixture(t)
	// Expansion measures deep changes even when display details are folded.
	c.MaxDepth = 2
	var err error
	containers, resources := base.Containers, base.Resources
	base, err = buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Containers, base.Resources = containers, resources
	before := httpapi.JSONText(base)
	oldOther := httpapi.JSONText(snapshotNodes(base.Tree)[other])
	mustWrite(t, filepath.Join(other, "unrelated.bin"), make([]byte, 65536))
	file := filepath.Join(target, "a", "b", "c", "d", "e", "file.bin")
	mustWrite(t, file, make([]byte, 32768))
	mustWrite(t, filepath.Join(target, "live.bin"), make([]byte, 16384))
	next, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotAccounting(t, next)
	if httpapi.JSONText(base) != before || httpapi.JSONText(snapshotNodes(next.Tree)[other]) != oldOther {
		t.Fatal("modified baseline or unrelated directory")
	}
	if next.Tree.Allocated-base.Tree.Allocated != 24576 {
		t.Fatalf("deep and direct file growth should both be measured: %d", next.Tree.Allocated-base.Tree.Allocated)
	}
	if n := snapshotNodes(next.Tree)[target+"/a"]; n == nil || n.SizeUnknown {
		t.Fatal("next directory must have measured usage")
	}
	if snapshotNodes(next.Tree)[target+"/a/b"] != nil {
		t.Fatal("retained below the requested display depth")
	}
	if next.Revision != 1 || next.FinishedAt != base.FinishedAt || next.UpdatedAt == "" {
		t.Fatal("incorrect revision/timestamps")
	}
	current := target
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		current = filepath.Join(current, name)
		next, err = expandForTest(context.Background(), next, c, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertSnapshotAccounting(t, next)
		last := next.Scan["last_incremental"].(object)
		if numberInt64(last["visited_entries"]) < 2 {
			t.Fatalf("recursive inspection missed entries: %v", last)
		}
	}
	if next.Tree.Allocated-base.Tree.Allocated != 24576 {
		t.Fatal("repeated drilldown counted deep growth twice")
	}
	os.Remove(file)
	next, err = expandForTest(context.Background(), next, c, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotAccounting(t, next)
	if next.Tree.Files != 2 {
		t.Fatal("deleted file count not propagated")
	}
	for _, container := range next.Containers {
		if container.WritableLayer == nil || *container.WritableLayer.Allocated != snapshotNodes(next.Tree)[target].Allocated {
			t.Fatal("stale writable-layer summary")
		}
	}
}
func TestIncrementalHardlinksDoNotExpandScope(t *testing.T) {
	for _, newLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "new"}[newLink], func(t *testing.T) {
			base, c, target, other := incrementalFixture(t)
			source, alias := filepath.Join(other, "unrelated.bin"), filepath.Join(target, "alias.bin")
			if !newLink {
				os.Link(source, alias)
				var err error
				base, err = buildSnapshot(context.Background(), c, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				os.Link(source, alias)
			}
			external := httpapi.JSONText(snapshotNodes(base.Tree)[other])
			mustWrite(t, source, make([]byte, 65536))
			next, err := expandForTest(context.Background(), base, c, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertSnapshotAccounting(t, next)
			if httpapi.JSONText(snapshotNodes(next.Tree)[other]) != external {
				t.Fatal("incremental scan updated an external directory")
			}
			path := next.Scan["last_incremental"].(object)["path"]
			if path != target {
				t.Fatalf("scope expanded: %v", path)
			}
			n := snapshotNodes(next.Tree)[alias]
			if n.Kind == "reference" && (n.Allocated != 0 || n.Reference != source) {
				t.Fatal("external hardlink counted twice")
			}
			os.Remove(source)
			again, err := expandForTest(context.Background(), next, c, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertSnapshotAccounting(t, again)
			if httpapi.JSONText(snapshotNodes(again.Tree)[other]) != external {
				t.Fatal("deleting an external link triggered a wider scan")
			}
		})
	}
}
func TestIncrementalFailureIsolated(t *testing.T) {
	base, c, target, _ := incrementalFixture(t)
	next, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateIncrementalNodes(snapshotNodes(next.Tree)); err != nil {
		t.Fatal(err)
	}
	before := httpapi.JSONText(next)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = expandForTest(ctx, next, c, target, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err = expandForTest(context.Background(), next, c, target, func(object) error { return errors.New("progress failed") }); err == nil {
		t.Fatal("ignored progress failure")
	}
	for _, p := range []string{"relative", "/proc/1", target + "/../other", target + "/not-retained"} {
		if _, err = expandForTest(context.Background(), next, c, p, nil); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	// A replaced directory cannot redirect the privileged helper outside the
	// recorded scope. Its failure leaves all totals and revision untouched.
	moved := target + "-moved"
	os.Rename(target, moved)
	os.Symlink(moved, target)
	if _, err = expandForTest(context.Background(), next, c, target, nil); err == nil {
		t.Fatal("followed replaced root symlink")
	}
	if httpapi.JSONText(next) != before {
		t.Fatal("failure changed baseline")
	}
}
func TestIncrementalWorkerPublicationPermissionsAndRevision(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	c := p.configure()
	dir := filepath.Join(p.storage, "nested")
	os.MkdirAll(filepath.Join(dir, "deeper"), 0700)
	mustWrite(t, filepath.Join(dir, "deeper", "data.bin"), make([]byte, 8192))
	c.MaxDepth = 1
	p.expect(200, "PUT", "/api/settings", object{"revision": 2, "value": c}, nil)
	job := p.expect(202, "POST", "/api/jobs", nil, nil)
	id := job["id"].(string)
	if result := waitJob(t, p.db, id); result["status"] != "completed" {
		t.Fatal(result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.m.mu.Lock()
		p.m.reapLocked()
		idle := p.m.process == nil
		p.m.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile(filepath.Join(p.db.Directory, "results", id, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	public := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
	if _, ok := public["incremental_accounting"]; ok {
		t.Fatal("private inode index leaked")
	}
	body := object{"path": dir, "revision": 0}
	for _, depth := range []any{0, 4, nil, "2"} {
		p.expect(400, "POST", "/api/jobs/"+id+"/expand", object{"path": dir, "revision": 0, "depth": depth}, nil)
	}
	p.expect(403, "POST", "/api/jobs/"+id+"/expand", body, map[string]string{"X-CSRF-Token": "bad"})
	p.expect(400, "POST", "/api/jobs/"+id+"/expand", object{"path": dir}, nil)
	p.expect(400, "POST", "/api/jobs/"+id+"/expand", object{"path": "/etc", "revision": 0}, nil)
	mustWrite(t, filepath.Join(dir, "deeper", "data.bin"), make([]byte, 32768))
	expansion := p.expect(202, "POST", "/api/jobs/"+id+"/expand", body, nil)
	planConfig := expansion["config"].(map[string]any)
	roots := planConfig["root"].([]any)
	if len(roots) != 1 || roots[0] != dir || planConfig["include_docker_root"] != false || planConfig["max_depth"] != float64(1) {
		t.Fatal("incremental job plan includes an unrelated scan root")
	}
	eid := expansion["id"].(string)
	if result := waitJob(t, p.db, eid); result["status"] != "completed" {
		t.Fatal(result)
	}
	updated := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
	if updated["job_id"] != id || numberInt64(updated["revision"]) < 2 {
		t.Fatal("original record identity or revision changed incorrectly")
	}
	originalFile, err := os.ReadFile(filepath.Join(p.db.Directory, "results", id, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(originalFile) != string(raw) {
		t.Fatal("immutable baseline overwritten")
	}
	history := p.expect(200, "GET", "/api/jobs/"+id, nil, nil)
	if history["allocated"] != updated["tree"].(map[string]any)["allocated"] || history["snapshot_revision"] != updated["revision"] {
		t.Fatal("history totals/version stale")
	}
	latest, err := p.db.latest()
	if err != nil || latest == nil || *latest != id {
		t.Fatal("incremental task replaced latest baseline")
	}
	agent, err := p.api.Service.ReadSnapshot(id)
	if err != nil || agent.Revision != numberInt64(updated["revision"]) || float64(agent.Tree.Allocated) != history["allocated"] {
		t.Fatalf("agent saw stale head: %v", err)
	}
	p.expect(409, "POST", "/api/jobs/"+id+"/expand", body, nil)
	p.expect(400, "POST", "/api/jobs/"+eid+"/expand", object{"path": dir, "revision": 0}, nil)
	p.expect(201, "POST", "/api/users", object{"username": "reader", "password": "A-reader-password-123", "role": "viewer"}, nil)
	p.login(false, "reader", "A-reader-password-123")
	p.expect(403, "POST", "/api/jobs/"+id+"/expand", object{"path": dir, "revision": 1}, nil)
}
func TestIncrementalCancelledPublicationLeavesHeadUntouched(t *testing.T) {
	p := newTestPlatform(t)
	base, c, target, _ := incrementalFixture(t)
	id, eid := platform.RandomHex(16), platform.RandomHex(16)
	_, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?),(?,'cancelling','incremental','test',2,?)", id, httpapi.JSONText(c), eid, httpapi.JSONText(c))
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), base); err != nil {
		t.Fatal(err)
	}
	result, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := scanPlan{BaseJobID: id, IncrementalPath: target}
	if err = p.db.publishDirectory(context.Background(), eid, plan, result, object{}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel publish: %v", err)
	}
	current, err := p.db.readSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 0 {
		t.Fatal("cancelled scan published")
	}
	// A stale optimistic version rolls back both job completion and the head.
	p.db.SQL.Exec("UPDATE jobs SET status='running' WHERE id=?", eid)
	plan.BaseRevision = 4
	if err = p.db.publishDirectory(context.Background(), eid, plan, result, object{}, true); err == nil {
		t.Fatal("stale update published")
	}
	j, _ := p.db.job(eid)
	if j["status"] != "running" {
		t.Fatal("failed commit completed child")
	}
	var decoded Snapshot
	saved, _ := p.db.readSnapshot(id)
	decoded = *saved
	if decoded.Tree.Allocated != base.Tree.Allocated {
		t.Fatal("baseline changed")
	}
}

func TestIncrementalAncestorDeviceDeltas(t *testing.T) {
	// Model a target spanning two real filesystems. Moving bytes between devices
	// must propagate each signed delta; assigning the whole delta to the target's
	// root device would keep the grand total right but corrupt disk reconciliation.
	leaf := &Node{Path: "/storage/target", Kind: "directory", Allocated: 80, Apparent: 160, Files: 4, Errors: 2, Excluded: 1, PermissionDenied: 1, OmittedReferences: 2, DeviceAllocated: map[string]int64{"1": 30, "2": 50}, Children: []*Node{}}
	sibling := &Node{Path: "/storage/sibling", Kind: "directory", Allocated: 220, Apparent: 440, Files: 10, DeviceAllocated: map[string]int64{"1": 70, "2": 150}, Children: []*Node{}}
	parent := &Node{Path: "/storage", Kind: "directory", Children: []*Node{leaf, sibling}}
	aggregate(parent, leaf)
	aggregate(parent, sibling)
	root := &Node{Path: "@root", Kind: "root", Children: []*Node{parent}}
	aggregate(root, parent)
	replacement := &Node{Path: leaf.Path, Kind: "directory", Allocated: 110, Apparent: 220, Files: 7, DeviceAllocated: map[string]int64{"1": 10, "2": 100}, Children: []*Node{}}
	previous := root
	root = applyDirectoryReplacement(root, leaf.Path, replacement)
	parent = root.Children[0]
	if previous.Allocated != 300 || previous.Children[0].Children[0] != leaf {
		t.Fatal("modified the baseline")
	}
	if err := validateIncrementalNodes(snapshotNodes(root)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*Node{parent, root} {
		if n.Allocated != 330 || n.Apparent != 660 || n.Files != 17 || n.Errors != 0 || n.Excluded != 0 || n.PermissionDenied != 0 || n.OmittedReferences != 0 || n.DeviceAllocated["1"] != 80 || n.DeviceAllocated["2"] != 250 {
			t.Fatalf("incorrect ancestor delta: %+v", n)
		}
	}
	if parent.Children[1] != sibling || sibling.Allocated != 220 {
		t.Fatal("changed unaffected sibling")
	}
}

func TestInspectDirectoriesReusesDockerHelper(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "data.bin"), make([]byte, 8192))
	c := defaultConfig()
	c.ScanBackend = "docker"
	expected, err := scanPhysical(context.Background(), helperRequest{Config: c, Paths: []string{dir}, StrictPaths: true}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	expected.Backend = "docker"
	fixture := installHelperDockerFixture(t, "success")
	scriptPath := filepath.Join(fixture, "docker")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	empty := httpapi.JSONText(helperEvent{Result: &physicalScan{Tree: &Node{Path: "@root", Kind: "root", Children: []*Node{}}, Backend: "docker", CanonicalPaths: map[string]string{}}})
	frame := httpapi.JSONText(helperEvent{Result: expected})
	safeFrame := strings.ReplaceAll(frame, "'", "'\"'\"'")
	mustWrite(t, scriptPath, []byte(strings.Replace(string(script), empty, safeFrame, 1)))
	input := DirectoryInspection{Paths: []string{dir}, RetainPaths: []string{filepath.Join(dir, "data.bin")}, Seed: []InodeRecord{{Device: 1, Inode: 2, Path: "/outside"}}}
	actual, err := InspectDirectories(context.Background(), c, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Backend != "docker" || actual.Accounting == nil || actual.Tree.Allocated != expected.Tree.Allocated {
		t.Fatal("helper result not reused")
	}
	raw, err := os.ReadFile(filepath.Join(fixture, "request"))
	if err != nil {
		t.Fatal(err)
	}
	var request helperRequest
	if json.Unmarshal(raw, &request) != nil || !request.StrictPaths || len(request.RetainPaths) != 1 || len(request.Seed) != 1 {
		t.Fatal("inspection context lost in helper request")
	}
	if _, err = os.Stat(filepath.Join(fixture, "removed")); err != nil {
		t.Fatal("helper not cleaned up")
	}
	// A helper result from a different canonical root cannot be merged.
	expected.Tree.Children[0].Path = "/changed-path"
	badFrame := strings.ReplaceAll(httpapi.JSONText(helperEvent{Result: expected}), "'", "'\"'\"'")
	mustWrite(t, scriptPath, []byte(strings.Replace(string(script), empty, badFrame, 1)))
	if _, err = InspectDirectories(context.Background(), c, input, nil); err == nil {
		t.Fatal("accepted changed scan root")
	}
}

func TestIncrementalPreservesRecordedExclusions(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	excluded := filepath.Join(target, "private")
	os.MkdirAll(excluded, 0700)
	mustWrite(t, filepath.Join(excluded, "data.bin"), make([]byte, 16384))
	mustWrite(t, filepath.Join(target, "visible.bin"), make([]byte, 8192))
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{root}
	c.MaxDepth = 1
	recorded := c
	recorded.Exclude = []string{excluded}
	base, err := buildSnapshot(context.Background(), recorded, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Roundtrip exercises the JSON-decoded []any form of scan.excludes; c models
	// saved settings without exclusions added by a previous worker at runtime.
	var persisted Snapshot
	if err = json.Unmarshal([]byte(httpapi.JSONText(base)), &persisted); err != nil {
		t.Fatal(err)
	}
	next, err := expandForTest(context.Background(), &persisted, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Tree.Allocated != base.Tree.Allocated || next.Tree.Excluded != 1 || snapshotNodes(next.Tree)[excluded].Kind != "excluded" {
		t.Fatal("recorded exclusion was lost")
	}
	for _, r := range next.Accounting {
		if within(r.Path, excluded) {
			t.Fatal("excluded files entered the inode index")
		}
	}
	assertSnapshotAccounting(t, next)
}

func TestDirectoryExpansionPreservesUnreadableHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary directory permissions")
	}
	base, c, target, _ := incrementalFixture(t)
	// Retain the child baseline, then make it unreadable.
	var err error
	base, err = expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	boundary := filepath.Join(target, "a")
	before := snapshotNodes(base.Tree)[boundary].Allocated
	if err := os.Chmod(boundary, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(boundary, 0700)
	next, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := snapshotNodes(next.Tree)[boundary]
	if child == nil || child.Allocated != before || !child.SizeUnknown || child.Errors == 0 {
		t.Fatal("unreadable history was reported as zero")
	}
}
func TestDirectoryExpansionMeasuresFoldedDescendants(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "selected")
	deep := filepath.Join(target, "nested", "data")
	unrelated := filepath.Join(root, "unrelated")
	os.MkdirAll(deep, 0700)
	os.Mkdir(unrelated, 0700)
	file, err := os.Create(filepath.Join(deep, "ten-gigabytes.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(10 << 30); err != nil {
		t.Fatal(err)
	}
	file.Close()
	for i := 0; i < 2000; i++ {
		mustWrite(t, filepath.Join(deep, strconv.Itoa(i)), []byte("data"))
	}
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{root}
	c.MaxDepth = 1
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	for depth := 1; depth <= 3; depth++ {
		c.MaxDepth = depth
		next, err := expandDirectory(context.Background(), base, c, target, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		last := next.Scan["last_incremental"].(object)
		if numberInt64(last["visited_entries"]) != 2004 {
			t.Fatalf("depth %d truncated physical traversal: %v", depth, last)
		}
		if next.Tree.Allocated != base.Tree.Allocated {
			t.Fatalf("depth %d changed measured usage", depth)
		}
		children := snapshotNodes(next.Tree)[deep]
		if depth == 1 && children != nil || depth == 2 && (children == nil || len(children.Children) != 0) || depth == 3 && (children == nil || len(children.Children) != 2001) {
			t.Fatalf("depth %d retained the wrong display detail", depth)
		}
	}
}

func TestLazyReplacedSharedOwnerKeepsOutsideClaimUnresolved(t *testing.T) {
	_, c, target, other := incrementalFixture(t)
	// Separate, sorted roots give deterministic ownership independent of readdir order.
	selected := filepath.Join(filepath.Dir(target), "a-selected")
	if err := os.Rename(target, selected); err != nil {
		t.Fatal(err)
	}
	target = selected
	c.Root = []string{target, other}
	owner := filepath.Join(target, "live.bin")
	alias := filepath.Join(other, "shared.bin")
	if err := os.Link(owner, alias); err != nil {
		t.Fatal(err)
	}
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Retain inspection identities to detect replacement of an externally referenced inode.
	physical, err := scanPhysical(context.Background(), helperRequest{Config: c, Paths: c.Root, StrictPaths: true}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	base.Accounting = physical.Accounting
	originalSize := snapshotNodes(base.Tree)[owner].Allocated
	var original InodeRecord
	for _, r := range base.Accounting {
		if r.Path == owner {
			original = r
		}
	}
	if snapshotNodes(base.Tree)[owner].Kind == "reference" {
		t.Fatal("fixture needs the selected file to own the inode")
	}
	external := httpapi.JSONText(snapshotNodes(base.Tree)[other])
	os.Remove(owner)
	mustWrite(t, owner, make([]byte, 65536))
	next := base
	for i := 0; i < 2; i++ {
		next, err = expandForTest(context.Background(), next, c, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertSnapshotAccounting(t, next)
		if httpapi.JSONText(snapshotNodes(next.Tree)[other]) != external {
			t.Fatal("outside directory was changed")
		}
		n := snapshotNodes(next.Tree)[owner]
		if n == nil || !n.SizeUnknown || n.Allocated != originalSize {
			t.Fatal("replaced inode silently retargeted the outside alias")
		}
		for _, r := range next.Accounting {
			if r.Path == owner && (r.Inode != original.Inode || r.Device != original.Device) {
				t.Fatal("historical shared inode was overwritten")
			}
		}
	}
}
