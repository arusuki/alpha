package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHostUsageAllowsMeasuredFoldedDirectoriesAndHostHardLinks(t *testing.T) {
	for _, depth := range []int{0, 1, 3} {
		root := t.TempDir()
		target, source := filepath.Join(root, "packages"), filepath.Join(root, "environment")
		for _, p := range []string{target, source} {
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
		}
		original := filepath.Join(target, "original")
		mustWrite(t, original, make([]byte, 8192))
		if err := os.Link(original, filepath.Join(source, "alias")); err != nil {
			t.Fatal(err)
		}
		c := defaultConfig()
		c.MaxDepth = depth
		scanner := newScanner(c, []MountInfo{}, nil)
		tree, err := scanner.Scan(context.Background(), []string{target, source})
		if err != nil {
			t.Fatal(err)
		}
		s := &Snapshot{Tree: tree, Scan: object{"omitted_references": scanner.OmittedReferences}}
		u := buildUsage(s)
		for _, p := range []string{target, source} {
			node := hostNodeSummary(u, u.Nodes[p])
			if node["host_only"] != true || node["host_allocated"] != node["allocated"] || node["container_allocated"] != int64(0) {
				t.Fatalf("depth %d rejected Host-only directory: %v", depth, node)
			}
		}
		// The same folded link must prevent cleanup as soon as the source is
		// associated with a container, even though the alias owns zero bytes.
		s.Containers = []Container{{ID: "container"}}
		s.Resources = []Resource{{Path: source, Containers: []string{"container"}}}
		u = buildUsage(s)
		if u.HostOnly[target] || u.HostOnly[source] {
			t.Fatalf("depth %d lost container hard-link protection", depth)
		}
	}
}

func TestHostFoldedReferenceSafetyGraph(t *testing.T) {
	for _, tc := range []struct {
		name, claim             string
		broken, partial, hidden bool
	}{
		{name: "host links"},
		{name: "container source", claim: "/source"},
		{name: "container target", claim: "/target"},
		{name: "zero-byte nested claim", claim: "/target/hidden/container"},
		{name: "hidden Host target", hidden: true},
		{name: "hidden container target", hidden: true, claim: "/target/hidden/container"},
		{name: "missing target", broken: true},
		{name: "partial target", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &Node{Path: "/target", Kind: "directory", Allocated: 8192, Omitted: 10, SizeUnknown: tc.partial}
			frontier := &Node{Path: "/target/hidden", Kind: "directory", Allocated: 4096, Omitted: 5}
			if !tc.hidden {
				target.Children = []*Node{frontier}
			}
			source := &Node{Path: "/source", Kind: "directory", Omitted: 1, OmittedReferences: 1, OmittedReferenceTargets: []string{frontier.Path}}
			if tc.broken {
				source.OmittedReferenceTargets = []string{"/missing/file"}
			}
			safe := &Node{Path: "/safe", Kind: "directory", Allocated: 4096, Omitted: 20}
			s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Allocated: 12288, Children: []*Node{target, source, safe}}}
			if tc.claim != "" {
				s.Containers = []Container{{ID: "container"}}
				s.Resources = []Resource{{Path: tc.claim, Containers: []string{"container"}}}
			}
			u := buildUsage(s)
			want := tc.claim == "" && !tc.broken && !tc.partial
			if u.HostOnly[source.Path] != want || !u.HostOnly[safe.Path] {
				t.Fatalf("unexpected source/sibling safety: %+v", u.HostBlockers)
			}
			if !want && u.HostBlockers[source.Path].Code == "" {
				t.Fatal("missing diagnostic blocker")
			}
			if tc.claim != "" && (u.HostOnly[target.Path] || (!tc.hidden && u.HostOnly[frontier.Path])) {
				t.Fatal("container uncertainty failed to protect target subtree")
			}
		})
	}
}

func TestHostFoldedReferencesAllowCyclesUntilAContainerClaimsOneSide(t *testing.T) {
	a := &Node{Path: "/a", Kind: "directory", OmittedReferences: 1, OmittedReferenceTargets: []string{"/b"}}
	b := &Node{Path: "/b", Kind: "directory", OmittedReferences: 1, OmittedReferenceTargets: []string{"/a"}}
	s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Children: []*Node{a, b}}}
	u := buildUsage(s)
	if !u.HostOnly[a.Path] || !u.HostOnly[b.Path] {
		t.Fatal("Host hard-link cycle was rejected")
	}
	s.Resources = []Resource{{Path: a.Path, Containers: []string{"unknown-container"}}}
	u = buildUsage(s)
	if u.HostOnly[a.Path] || u.HostOnly[b.Path] {
		t.Fatal("unknown container claim failed to propagate")
	}
}

func TestHostRetainedReferenceCanResolveMeasuredHiddenTarget(t *testing.T) {
	packages := &Node{Path: "/env/packages", Kind: "directory", Allocated: 8192, Omitted: 10}
	alias := &Node{Path: "/env/etc/config", Kind: "reference", Reference: "/env/packages/pkg/etc/config"}
	env := &Node{Path: "/env", Kind: "directory", Allocated: 8192, Children: []*Node{packages, alias}}
	s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Allocated: 8192, Children: []*Node{env}}}
	u := buildUsage(s)
	if !u.HostOnly[env.Path] {
		t.Fatalf("measured hidden target rejected: %v", u.HostBlockers)
	}
	s.Containers = []Container{{ID: "container"}}
	s.Resources = []Resource{{Path: alias.Path, Containers: []string{"container"}}}
	u = buildUsage(s)
	if u.HostOnly[packages.Path] {
		t.Fatal("hidden target lost retained alias container claim")
	}
}

func TestHostBlockerReportsActualPermissionFailure(t *testing.T) {
	denied := &Node{Path: "/home/user/private", Kind: "unreadable", Errors: 1, PermissionDenied: 1}
	cache := &Node{Path: "/home/user/cache", Kind: "directory", Allocated: 4096, Omitted: 20}
	home := &Node{Path: "/home/user", Kind: "directory", Allocated: 4096, Errors: 1, PermissionDenied: 1, Children: []*Node{denied, cache}}
	u := buildUsage(&Snapshot{Tree: &Node{Path: "@root", Kind: "root", Allocated: 4096, Errors: 1, PermissionDenied: 1, Children: []*Node{home}}})
	result := hostNodeSummary(u, home)
	blocker, ok := result["host_only_blocker"].(hostBlocker)
	if result["host_only"] != false || !ok || blocker.Path != denied.Path || blocker.Code != "permission_denied" || !u.HostOnly[cache.Path] {
		t.Fatalf("wrong failure or unrelated sibling blocked: %v", result)
	}
}
