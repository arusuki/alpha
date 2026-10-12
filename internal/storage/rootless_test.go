package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/containerorigin"
	"project-alpha/internal/httpapi"
)

func diskOriginSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "disk-origin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestRootlessDiskDiscoveryAccountingAndOwnership(t *testing.T) {
	parent, child := strings.Repeat("a", 64), strings.Repeat("b", 64)
	base := t.TempDir()
	rootful, rootless, shared := filepath.Join(base, "docker"), filepath.Join(base, "rootless"), filepath.Join(base, "shared")
	for _, p := range []string{rootful + "/upper", rootless + "/upper", rootless + "/cache", shared} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p+"/data", make([]byte, 8192))
	}
	// The same inode appears in two engines, and both also bind the same source.
	if err := os.Link(rootful+"/upper/data", rootless+"/upper/alias"); err != nil {
		t.Fatal(err)
	}
	socket := diskOriginSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != containerorigin.Endpoint || r.URL.Query().Get("container_id") != child {
			t.Error(r.URL.String())
		}
		json.NewEncoder(w).Encode(containerorigin.Response{Version: 1, Origins: []containerorigin.Record{{Version: 1, ContainerID: child, Source: containerorigin.Source{PID: 123, StartTicks: 1, ContainerID: parent}}}})
	})
	ctx := context.WithValue(context.Background(), rootlessSocketKey{}, socket)
	command := func(_ context.Context, args []string, _ int) (string, error) {
		if args[0] == "context" {
			return `[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`, nil
		}
		if args[0] != "--host" {
			t.Fatalf("unbound endpoint: %v", args)
		}
		id, root, daemon := parent, rootful, "rootful"
		if args[1] == "unix://"+socket {
			id, root, daemon = child, rootless, "rootless"
		} else if args[1] != "unix:///var/run/docker.sock" {
			t.Fatal(args)
		}
		args = args[2:]
		switch args[0] {
		case "info":
			return httpapi.JSONText(object{"ID": daemon, "DockerRootDir": root, "SecurityOptions": []string{}}), nil
		case "ps":
			return id + "\t" + daemon, nil
		case "container":
			return httpapi.JSONText([]object{{"Id": id, "Name": "/" + daemon, "State": object{"Status": "exited"}, "Config": object{"Labels": object{"project-alpha.owner": "forged-member"}}, "GraphDriver": object{"Data": object{"UpperDir": root + "/upper"}}, "SizeRw": 999999999, "Mounts": []object{{"Type": "bind", "Source": shared, "Destination": "/shared"}}}}), nil
		case "volume":
			return "", nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	c := defaultConfig()
	c.ScanBackend = "host"
	c.MaxDepth = 10
	c.IncludeDockerRoot = true
	snapshot, err := buildSnapshotWithDocker(ctx, c, nil, command)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Containers) != 2 || len(dockerEngines(snapshot.Docker)) != 2 {
		t.Fatalf("engines missing: %+v", snapshot)
	}
	if row := snapshot.Containers[1]; !row.Rootless || row.Owner != containerorigin.RootlessOwner || row.OriginContainerID != parent {
		t.Fatalf("trusted audit not used: %+v", row)
	}
	found := false
	for _, r := range snapshot.Resources {
		if r.Path == shared {
			found = true
			if len(r.Containers) != 2 {
				t.Fatal(r)
			}
		}
	}
	if !found {
		t.Fatal("shared bind missing")
	}
	ApplyContainerOwners(snapshot.Containers, map[string]string{parent: "alice"})
	if snapshot.Containers[1].Owner != "alice" {
		t.Fatal(snapshot.Containers)
	}
	u := buildUsage(snapshot)
	if u.CrossOwner != 0 || u.Shared == 0 || u.Unrelated == 0 || u.Exclusive+u.Shared+u.Unrelated != snapshot.Tree.Allocated {
		t.Fatalf("bad accounting: %+v", u)
	}
	// Logical SizeRw is not added to physical bytes; each inode is charged once.
	if snapshot.Tree.Allocated >= 999999999 {
		t.Fatal("logical writable size duplicated")
	}
	n := snapshotNodes(snapshot.Tree)[rootless+"/upper/alias"]
	if n == nil || n.Kind != "reference" || n.Allocated != 0 {
		t.Fatalf("hard link not deduplicated: %+v", n)
	}
	if u.HostOnly[rootless+"/cache"] {
		t.Fatal("engine cache was classified as deletable host data")
	}
	if err := validateCleanupPath(snapshot, "/alpha", nil, nil, rootless+"/cache/data"); err == nil {
		t.Fatal("raw rootless data became deletable")
	}
	ApplyContainerOwners(snapshot.Containers, map[string]string{parent: "alice", child: "bob"})
	if split := buildUsage(snapshot); split.CrossOwner == 0 || split.Shared != u.Shared || split.Exclusive+split.Shared+split.Unrelated != snapshot.Tree.Allocated {
		t.Fatal("cross-owner sharing duplicated or lost bytes")
	}
	// The source's current owner controls every child without assigning labels.
	ApplyContainerOwners(snapshot.Containers, map[string]string{parent: "bob"})
	if _, err := memberDiskView(snapshot, "alice", child, "", 0); err == nil {
		t.Fatal("old member retained access")
	}
	if _, err := memberDiskView(snapshot, "bob", child, "", 0); err != nil {
		t.Fatal(err)
	}
	ApplyContainerOwners(snapshot.Containers, map[string]string{})
	if snapshot.Containers[1].Owner != containerorigin.RootlessOwner {
		t.Fatal("stale attribution retained")
	}
	// Snapshot serialization preserves provenance and both daemon identities.
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = validateSnapshotDocker(decoded.Docker); err != nil {
		t.Fatal(err)
	}
	if err = validateSnapshotOrigins(&decoded); err != nil {
		t.Fatal(err)
	}
	ApplyContainerOwners(decoded.Containers, map[string]string{parent: "carol"})
	if decoded.Containers[1].Owner != "carol" {
		t.Fatal("roundtrip lost provenance")
	}
}

func TestRootlessDiskAuditFailureAndDaemonDedup(t *testing.T) {
	id := strings.Repeat("a", 64)
	socket := diskOriginSocket(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "audit damaged", 500) })
	ctx := context.WithValue(context.Background(), rootlessSocketKey{}, socket)
	fixture := dockerFixture(t, true)
	command := func(ctx context.Context, args []string, timeout int) (string, error) {
		if len(args) > 1 && args[0] == "--host" {
			args = append([]string{"--host", "unix:///var/run/docker.sock"}, args[2:]...)
		}
		return fixture(ctx, args, timeout)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	metadata, rows, _, warnings, err := discoverStorage(ctx, defaultConfig(), command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(dockerEngines(metadata)) != 1 || rows[0].ID != id || !rows[0].Rootless || rows[0].Owner != containerorigin.RootlessOwner || rows[0].OriginContainerID != "" {
		t.Fatalf("dedup or unknown attribution failed: %+v", rows)
	}
	if !strings.Contains(httpapi.JSONText(warnings), "审计不可读") {
		t.Fatal("silent audit failure")
	}
	broken := func(ctx context.Context, args []string, timeout int) (string, error) {
		if len(args) > 1 && args[1] == "unix://"+socket {
			return "", errors.New("engine offline")
		}
		return fixture(ctx, args, timeout)
	}
	if _, _, _, _, err = discoverStorage(ctx, defaultConfig(), broken, nil); err == nil {
		t.Fatal("published incomplete engine inventory")
	}
	ctx = context.WithValue(context.Background(), rootlessSocketKey{}, socket+"-missing")
	_, rows, _, _, err = discoverStorage(ctx, defaultConfig(), fixture, nil)
	if err != nil || len(rows) != 1 || rows[0].Rootless {
		t.Fatalf("optional service absence failed: %v %+v", err, rows)
	}
}

func TestRootlessSnapshotOwnersUseLiveOverlay(t *testing.T) {
	p := newTestPlatform(t)
	parent, child := strings.Repeat("a", 64), strings.Repeat("b", 64)
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Containers[1].Rootless = true
	snapshot.Containers[1].OriginContainerID = parent
	id := strings.Repeat("f", 32)
	if _, err = p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','test',1,2,?)", id, httpapi.JSONText(defaultConfig())); err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob", ""} {
		if err = p.db.setOwner(parent, owner, "test"); err != nil {
			t.Fatal(err)
		}
		want := owner
		if want == "" {
			want = containerorigin.RootlessOwner
		}
		result, err := p.api.ReadSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if result.Containers[1].Owner != want {
			t.Fatal(result.Containers[1])
		}
		exported, err := p.api.Snapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		row := exported["containers"].([]any)[1].(map[string]any)
		if row["id"] != child || row["owner"] != want {
			t.Fatal(row)
		}
	}
}
