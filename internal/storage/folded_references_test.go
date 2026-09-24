package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestFoldedReferencesLimitOnlyAffectedHostTrees(t *testing.T) {
	for _, depth := range []int{0, 1} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			root := t.TempDir()
			target, aliases, host := filepath.Join(root, "a-target"), filepath.Join(root, "b-aliases"), filepath.Join(root, "c-host")
			for _, dir := range []string{target, filepath.Join(aliases, "nested"), host} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(target, "shared")
			mustWrite(t, file, make([]byte, 8192))
			for i := 0; i < 128; i++ {
				if err := os.Link(file, filepath.Join(aliases, "nested", fmt.Sprint(i))); err != nil {
					t.Fatal(err)
				}
			}
			c := defaultConfig()
			c.MaxDepth = depth
			scanner := newScanner(c, []MountInfo{}, nil)
			tree, err := scanner.Scan(context.Background(), []string{target, aliases, host})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := &Snapshot{Tree: tree, Scan: object{"omitted_references": scanner.OmittedReferences, "lazy_accounting_limited": true}}
			snapshot.Containers = []Container{{ID: "container"}}
			snapshot.Resources = []Resource{{Path: aliases, Containers: []string{"container"}}}
			usage := buildUsage(snapshot)
			if scanner.OmittedReferences != 128 || !usage.Limited || !usage.HostOnly[host] || usage.HostOnly[target] || usage.HostOnly[aliases] {
				t.Fatalf("uncertainty leaked or shared target certified: omitted=%d host=%v", scanner.OmittedReferences, usage.HostOnly)
			}
			source, destination := aliases, file
			if depth == 1 {
				source, destination = filepath.Join(aliases, "nested"), file
			}
			if got := usage.Nodes[source].OmittedReferenceTargets; !reflect.DeepEqual(got, []string{destination}) {
				t.Fatalf("folded links lost or duplicated: %v", got)
			}
			// The annotation is persisted with the source node, not global state.
			before := httpapi.JSONText(snapshot)
			c.NoDocker, c.ScanBackend, c.MaxDepth = true, "host", 1
			next, err := expandDirectory(context.Background(), snapshot, c, target, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if buildUsage(next).HostOnly[target] || !buildUsage(next).HostOnly[host] || httpapi.JSONText(snapshot) != before {
				t.Fatal("local refresh lost outside alias protection or changed the baseline")
			}
		})
	}
}

func TestFoldedReferencesToSeededOutsideTargets(t *testing.T) {
	root := t.TempDir()
	outside, inside := filepath.Join(root, "outside"), filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, outside, make([]byte, 8192))
	if err := os.Link(outside, filepath.Join(inside, "alias")); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.NoDocker, c.ScanBackend, c.MaxDepth = true, "host", 0
	first, err := InspectDirectories(context.Background(), c, DirectoryInspection{Paths: []string{outside}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InspectDirectories(context.Background(), c, DirectoryInspection{Paths: []string{inside}, Seed: first.Accounting}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := second.Tree.Children[0]
	if !reflect.DeepEqual(source.OmittedReferenceTargets, []string{outside}) {
		t.Fatalf("outside target lost: %+v", source)
	}
	first.Tree.Children = append(first.Tree.Children, source)
	first.Tree.Allocated += source.Allocated
	u := buildUsage(&Snapshot{Tree: first.Tree, Containers: []Container{{ID: "container"}}, Resources: []Resource{{Path: inside, Containers: []string{"container"}}}})
	if u.HostOnly[outside] {
		t.Fatal("folding an outside alias certified its physical target")
	}
}

func TestHostUsageLocalUncertaintyAndTransitiveAliases(t *testing.T) {
	target := &Node{Path: "/target", Kind: "directory", Children: []*Node{{Path: "/target/file", Kind: "file"}}}
	alias := &Node{Path: "/alias", Kind: "reference", Reference: target.Path}
	source := &Node{Path: "/folded", Kind: "directory", OmittedReferences: 1, OmittedReferenceTargets: []string{alias.Path}}
	safe := &Node{Path: "/safe", Kind: "directory"}
	s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Children: []*Node{target, alias, source, safe}}, Scan: object{"omitted_references": 1, "lazy_accounting_limited": true}}
	s.Resources = []Resource{{Path: source.Path, Containers: []string{"unknown-container"}}}
	u := buildUsage(s)
	if !u.HostOnly[safe.Path] || u.HostOnly[target.Path] || u.HostOnly["/target/file"] || u.HostOnly[alias.Path] {
		t.Fatalf("uncertainty did not follow an alias into its subtree: %v", u.HostOnly)
	}
	partial := mergeDirectoryObservation(source, &Node{Path: source.Path, Kind: "directory", Scanning: true})
	if !reflect.DeepEqual(partial.OmittedReferenceTargets, source.OmittedReferenceTargets) {
		t.Fatal("partial observation forgot unvisited aliases")
	}
}

func TestFoldedReferenceEvidenceValidation(t *testing.T) {
	for _, targets := range [][]string{nil, {"relative"}, {"/a", "/a"}, {"/b", "/a"}} {
		n := &Node{Path: "/source", Kind: "directory", OmittedReferences: 1, OmittedReferenceTargets: targets}
		if err := validateIncrementalNodes(map[string]*Node{n.Path: n}); err == nil {
			t.Fatalf("invalid folded evidence accepted: %v", targets)
		}
	}
}

func TestHostUsageDoesNotCertifyDescendantsOfUnfinishedScan(t *testing.T) {
	child := &Node{Path: "/scanning/file", Kind: "file"}
	branch := &Node{Path: "/scanning", Kind: "directory", Children: []*Node{child}}
	safe := &Node{Path: "/safe", Kind: "directory"}
	s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Children: []*Node{branch, safe}}, Scan: object{"lazy_accounting_limited": true}}
	for _, scanning := range []bool{true, false} {
		branch.Scanning, branch.SizeUnknown = scanning, !scanning
		u := buildUsage(s)
		if u.HostOnly[child.Path] || !u.HostOnly[safe.Path] {
			t.Fatalf("unfinished branch certified or unrelated tree blocked: %v", u.HostOnly)
		}
	}
}

func TestFoldedReferenceEvidencePersistsWhenTotalsDoNotChange(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err := db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES('base','completed','manual','test',1,'{}')"); err != nil {
		t.Fatal(err)
	}
	n := &Node{Path: "/source", Kind: "directory", OmittedReferences: 1, OmittedReferenceTargets: []string{"/source/hidden"}}
	base := &Snapshot{SchemaVersion: snapshotVersion, Tree: &Node{Path: "@root", Kind: "root", OmittedReferences: 1, Children: []*Node{n, {Path: "/target", Kind: "file"}}}}
	base.Resources = []Resource{{Path: n.Path, Containers: []string{"unknown-container"}}}
	if err := db.Transaction(func(tx *sql.Tx) error { return storeSnapshot(tx, "base", base.Tree.Path, base, nil) }); err != nil {
		t.Fatal(err)
	}
	next := *base
	next.Revision = 1
	changed := copyNode(n)
	changed.OmittedReferenceTargets = []string{"/target"}
	next.Tree = applyDirectoryReplacement(base.Tree, n.Path, changed)
	if err := db.Transaction(func(tx *sql.Tx) error { return storeSnapshot(tx, "base", next.Tree.Path, &next, base) }); err != nil {
		t.Fatal(err)
	}
	stored, err := db.storedSnapshot("base")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshotNodes(stored.Tree)[n.Path].OmittedReferenceTargets, changed.OmittedReferenceTargets) || buildUsage(stored).HostOnly["/target"] {
		t.Fatal("unchanged totals hid changed reference evidence")
	}
}

func TestShallowHostRefreshRetainsContainerResourceBoundaries(t *testing.T) {
	base, c, target, _ := incrementalFixture(t)
	// The deeper source was omitted from the baseline, as in the failed Host
	// report after its root rescan. Its resource row is still available.
	deep := filepath.Join(target, "a", "b", "c", "d", "e")
	base.Containers = []Container{{ID: "container", UpperPath: &deep}}
	base.Resources = []Resource{{Path: deep, Containers: []string{"container"}, Kinds: []string{"writable"}}}
	c.MaxDepth, c.ScanBackend = 1, "host"
	next, err := expandDirectory(context.Background(), base, c, target, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := buildUsage(next)
	if u.Nodes[deep] == nil || u.ContainerAllocated[deep] == 0 || u.HostOnly[deep] || u.Containers["container"].Container.WritableLayer.Allocated == nil {
		t.Fatalf("shallow refresh erased container accounting: %+v", u.Containers["container"])
	}
}
