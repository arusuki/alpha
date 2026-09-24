package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestHostUsageSeparatesClaimsAndRejectsMixedTrees(t *testing.T) {
	host := &Node{Name: "host", Path: "/srv/host", Kind: "file", Allocated: 60}
	container := &Node{Name: "container", Path: "/srv/container", Kind: "directory", Allocated: 40}
	unknown := &Node{Name: "unknown", Path: "/srv/unknown", Kind: "file", Allocated: 20}
	empty := &Node{Name: "empty", Path: "/srv/empty", Kind: "directory", Allocated: 0}
	shared := &Node{Name: "srv", Path: "/srv", Kind: "directory", Allocated: 120, Children: []*Node{host, container, unknown, empty}}
	root := &Node{Name: "/", Path: "@root", Kind: "root", Allocated: 120, Children: []*Node{shared}}
	snapshot := &Snapshot{Tree: root, Containers: []Container{{ID: "known"}}, Resources: []Resource{
		{Path: container.Path, Containers: []string{"known"}},
		{Path: empty.Path, Containers: []string{"known"}},
		{Path: unknown.Path, Containers: []string{"missing"}},
	}}
	u := buildUsage(snapshot)
	if u.HostAllocated[shared.Path] != 80 || u.ContainerAllocated[shared.Path] != 40 || u.Unrelated != 80 {
		t.Fatalf("host/container accounting differs from unrelated: host=%d container=%d unrelated=%d", u.HostAllocated[shared.Path], u.ContainerAllocated[shared.Path], u.Unrelated)
	}
	if !u.HostOnly[host.Path] || u.HostOnly[shared.Path] || u.HostOnly[container.Path] || u.HostOnly[unknown.Path] || u.HostOnly[empty.Path] {
		t.Fatalf("unsafe host-only classification: %+v", u.HostOnly)
	}
	view := hostDirectoryResult(snapshot, u, shared.Path, 0, 10)
	if view["node"].(object)["host_only"] != false || view["self_and_omitted_host_allocated"] != int64(0) {
		t.Fatalf("mixed parent appeared safe: %v", view)
	}
	entries := view["entries"].([]object)
	if len(entries) != 4 || entries[0]["path"] != host.Path || entries[0]["host_allocated"] != int64(60) || entries[0]["host_only"] != true {
		t.Fatalf("host directory ranking or evidence incorrect: %v", entries)
	}
}

func TestHostCleanupRechecksOwnershipUnderStorageLock(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		content    []byte
	}{
		{name: "mixed parent", file: "owned.bin", content: make([]byte, 8192)},
		{name: "zero-byte claim", file: "owned.empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			owned := filepath.Join(p.storage, tc.file)
			if err := os.WriteFile(owned, tc.content, 0600); err != nil {
				t.Fatal(err)
			}
			job := p.expect(202, "POST", "/api/jobs", nil, nil)
			id := job["id"].(string)
			if done := waitJob(t, p.db, id); done["status"] != "completed" {
				t.Fatalf("scan failed: %v", done)
			}
			if err := p.api.Service.Wait(context.Background(), id, nil); err != nil {
				t.Fatal(err)
			}
			before, err := p.api.Service.ReadSnapshot(id)
			if err != nil {
				t.Fatal(err)
			}
			if !buildUsage(before).HostOnly[p.storage] {
				t.Fatal("test source was not initially a Host-only path")
			}
			changed := *before
			changed.Revision++
			changed.Containers = []Container{{ID: strings.Repeat("a", 64)}}
			changed.Resources = append(append([]Resource{}, before.Resources...), Resource{Path: owned, Containers: []string{changed.Containers[0].ID}})
			if err := p.db.Transaction(func(tx *sql.Tx) error { return storeSnapshot(tx, id, changed.Tree.Path, &changed, before) }); err != nil {
				t.Fatal(err)
			}
			called := false
			password := []byte("not-a-real-password")
			err = p.api.Service.DeleteHostReportPaths(context.Background(), "administrator", id, []string{p.storage}, password, func(string, string, string) error {
				called = true
				return nil
			})
			var apiErr *httpapi.Error
			if !errors.As(err, &apiErr) || apiErr.Status != 409 || called {
				t.Fatalf("unsafe Host deletion was not rejected before helper: err=%v callback=%v", err, called)
			}
			for _, b := range password {
				if b != 0 {
					t.Fatal("sudo password was retained after rejection")
				}
			}
			if _, err := os.Stat(owned); err != nil {
				t.Fatal("rejected cleanup removed the claimed path", err)
			}
		})
	}
}

func TestHostUsageProtectsDockerManagedDataWithoutContainerClaims(t *testing.T) {
	image := &Node{Name: "image", Path: "/var/lib/docker/image", Kind: "directory", Allocated: 40}
	docker := &Node{Name: "docker", Path: "/var/lib/docker", Kind: "directory", Allocated: 40, Children: []*Node{image}}
	varDir := &Node{Name: "lib", Path: "/var/lib", Kind: "directory", Allocated: 40, Children: []*Node{docker}}
	host := &Node{Name: "cache", Path: "/srv/cache", Kind: "file", Allocated: 20}
	srv := &Node{Name: "srv", Path: "/srv", Kind: "directory", Allocated: 20, Children: []*Node{host}}
	snapshot := &Snapshot{Tree: &Node{Name: "/", Path: "@root", Kind: "root", Allocated: 60, Children: []*Node{varDir, srv}}, Docker: object{"root": docker.Path}, Resources: []Resource{{Path: docker.Path, Kinds: []string{"docker-root"}}}}
	u := buildUsage(snapshot)
	if u.Unrelated != 60 || u.HostAllocated[docker.Path] != 40 || u.HostOnly[image.Path] || u.HostOnly[docker.Path] || u.HostOnly[varDir.Path] || !u.HostOnly[host.Path] {
		t.Fatalf("Docker-managed data became a host cleanup candidate: %+v", u.HostOnly)
	}
	snapshot.Docker = nil
	snapshot.Resources = []Resource{{Path: docker.Path, Kinds: []string{"host"}}}
	withoutDiscovery := buildUsage(snapshot)
	if withoutDiscovery.HostOnly[image.Path] || withoutDiscovery.HostOnly[docker.Path] {
		t.Fatalf("default Docker root became Host-only when discovery was disabled: %+v", withoutDiscovery.HostOnly)
	}
}

func TestHostUsageProtectsCanonicalDockerRoot(t *testing.T) {
	base := t.TempDir()
	actual := filepath.Join(base, "actual")
	alias := filepath.Join(base, "docker-link")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	aliases := dockerRootAliases(alias)
	if len(aliases) != 2 || aliases[0] != alias || aliases[1] != actual {
		t.Fatalf("Docker root alias was not canonicalized: %v", aliases)
	}
	node := &Node{Name: "actual", Path: actual, Kind: "directory", Allocated: 4096}
	snapshot := &Snapshot{Tree: &Node{Name: "/", Path: "@root", Kind: "root", Allocated: 4096, Children: []*Node{node}}, Docker: object{"root": alias}}
	if usage := buildUsage(snapshot); usage.HostOnly[actual] {
		t.Fatal("canonical Docker data root became a Host cleanup candidate")
	}
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	snapshot.Docker["root_canonical"] = actual
	if usage := buildUsage(snapshot); usage.HostOnly[actual] {
		t.Fatal("saved physical Docker root was lost after its alias changed")
	}
}

func TestHostRecordQueriesExposePagedRootsAndSafeNodes(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", nil, nil)
	id := job["id"].(string)
	if done := waitJob(t, p.db, id); done["status"] != "completed" {
		t.Fatalf("scan failed: %v", done)
	}
	roots, err := p.api.Service.Query(id, "host_roots", map[string]json.RawMessage{"offset": json.RawMessage(`0`), "limit": json.RawMessage(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	items := roots["items"].([]object)
	if roots["total"] != 1 || roots["has_more"] != false || len(items) != 1 || items[0]["path"] != p.storage || numberInt64(items[0]["host_allocated"]) <= 0 {
		t.Fatalf("host root response: %v", roots)
	}
	empty, err := p.api.Service.Query(id, "host_roots", map[string]json.RawMessage{"offset": json.RawMessage(`1`), "limit": json.RawMessage(`1`)})
	if err != nil || len(empty["items"].([]object)) != 0 {
		t.Fatalf("host roots pagination: %v %v", empty, err)
	}
	fields := map[string]json.RawMessage{"path": json.RawMessage(httpapi.JSONText(p.storage))}
	directory, err := p.api.Service.Query(id, "host_directory", fields)
	if err != nil {
		t.Fatal(err)
	}
	if directory["node"].(object)["host_only"] != true || len(directory["entries"].([]object)) == 0 {
		t.Fatalf("host directory response: %v", directory)
	}
	file := filepath.Join(p.storage, "model.bin")
	nodes, err := p.api.Service.Query(id, "host_nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText([]string{file, filepath.Join(p.storage, "missing"), "/proc"}))})
	if err != nil {
		t.Fatal(err)
	}
	rows := nodes["items"].([]object)
	if rows[0]["node"].(object)["host_only"] != true || rows[0]["node"].(object)["container_allocated"] != int64(0) || rows[1]["node"].(object)["known"] != false || rows[1]["node"].(object)["host_only"] != false || rows[2]["error"] == nil {
		t.Fatalf("host node validation evidence: %v", rows)
	}
	before, err := p.api.Service.ReadSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	changed := *before
	changed.Revision++
	changed.Resources = []Resource{{Path: p.storage, Kinds: []string{"docker-root"}}}
	if err := p.db.Transaction(func(tx *sql.Tx) error { return storeSnapshot(tx, id, changed.Tree.Path, &changed, before) }); err != nil {
		t.Fatal(err)
	}
	roots, err = p.api.Service.Query(id, "host_roots", nil)
	if err != nil || roots["total"] != 0 || len(roots["items"].([]object)) != 0 {
		t.Fatalf("Docker-only scan was planned as Host report roots: %v %v", roots, err)
	}
}

func TestHostUsageAllowsLinksWithoutLosingOwnershipProtection(t *testing.T) {
	for _, tc := range []struct {
		name, kind, claim                    string
		missing, cycle, incomplete, wantSafe bool
	}{
		{name: "symlink", kind: "symlink", wantSafe: true},
		{name: "symlink does not follow container target", kind: "symlink", claim: "target", wantSafe: true},
		{name: "host inode alias", kind: "reference", wantSafe: true},
		{name: "container target", kind: "reference", claim: "target"},
		{name: "container alias", kind: "reference", claim: "alias"},
		{name: "unknown target owner", kind: "reference", claim: "unknown"},
		{name: "missing target", kind: "reference", missing: true},
		{name: "reference cycle", kind: "reference", cycle: true},
		{name: "incomplete target", kind: "reference", incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &Node{Name: "target", Path: "/srv/target", Kind: "file", Allocated: 64, SizeUnknown: tc.incomplete}
			alias := &Node{Name: "alias", Path: "/srv/cache/alias", Kind: tc.kind, Reference: target.Path}
			if tc.missing {
				alias.Reference = "/missing"
			}
			if tc.cycle {
				target.Kind, target.Reference = "reference", alias.Path
			}
			cache := &Node{Name: "cache", Path: "/srv/cache", Kind: "directory", Allocated: 4096, Children: []*Node{alias}}
			s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Allocated: 4160, Children: []*Node{target, cache}}, Containers: []Container{{ID: "known"}}}
			switch tc.claim {
			case "target":
				s.Resources = []Resource{{Path: target.Path, Containers: []string{"known"}}}
			case "alias":
				s.Resources = []Resource{{Path: alias.Path, Containers: []string{"known"}}}
			case "unknown":
				s.Resources = []Resource{{Path: target.Path, Containers: []string{"missing"}}}
			}
			u := buildUsage(s)
			if tc.claim == "alias" && (u.HostAllocated[target.Path] != 0 || u.ContainerAllocated[target.Path] != 64 || u.HostOnly[target.Path]) {
				t.Fatal("container alias did not transfer ownership and bytes to its physical target")
			}
			if u.HostOnly[cache.Path] != tc.wantSafe {
				t.Fatalf("parent host_only = %v, want %v", u.HostOnly[cache.Path], tc.wantSafe)
			}
			if hostNodeSummary(u, alias)["host_only"] != false {
				t.Fatal("link advertised as a direct cleanup candidate")
			}
		})
	}
}
