package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPhysicalProgressTracksSharedContainersAndCapacity(t *testing.T) {
	root := t.TempDir()
	shared, nested, upper := filepath.Join(root, "shared"), filepath.Join(root, "shared", "nested"), filepath.Join(root, "upper")
	for _, path := range []string{nested, upper} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(path, "data"), make([]byte, 8192))
	}
	if err := os.Link(filepath.Join(upper, "data"), filepath.Join(shared, "hardlink")); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.MaxDepth, c.MaxNodes = 0, 1
	request := helperRequest{Config: c, Paths: []string{root, shared, nested, upper}, Mounts: []MountInfo{}, Containers: []scanContainer{{"a", "alice"}, {"b", "bob"}}, Resources: []Resource{
		{Path: shared, Containers: []string{"a", "b"}},
		{Path: nested, Containers: []string{"b"}},
		{Path: upper, Containers: []string{"a"}},
		{Path: shared, Containers: []string{"a"}}, // Duplicate resource metadata.
	}}
	var events []object
	r, err := scanPhysical(context.Background(), request, func(v object) error { events = append(events, v); return nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tree.Allocated != scanForTest(t, c, []string{root}).Allocated {
		t.Fatal("progress tracking changed deduplicated bytes")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	block := fs.Frsize
	if block == 0 {
		block = fs.Bsize
	}
	wantCapacity := fs.Blocks * uint64(block)
	lastBytes, lastDone := int64(0), 0
	sharedSeen, aliceSeen, hostSeen := false, false, false
	for _, v := range events {
		if v["capacity_known"] != true || v["capacity_total"] != wantCapacity {
			t.Fatalf("capacity missing or repeated for overlapping paths: %v", v)
		}
		if rows := v["capacity_filesystems"].([]object); len(rows) != 1 || rows[0]["total"] != wantCapacity {
			t.Fatalf("per-filesystem capacity missing or repeated: %v", rows)
		}
		allocated, done, remaining := v["allocated"].(int64), v["containers_done"].(int), v["containers_remaining"].(int)
		if allocated < lastBytes || done < lastDone || done+remaining != 2 {
			t.Fatalf("non-monotonic byte or container progress: %v", v)
		}
		lastBytes, lastDone = allocated, done
		current := v["current_containers"].([]scanContainer)
		if v["path"] == shared || v["path"] == nested {
			sharedSeen = sharedSeen || v["phase"] == "container" && len(current) == 2 && current[0].Name == "alice" && current[1].Name == "bob"
		}
		if v["path"] == upper {
			aliceSeen = v["phase"] == "container" && len(current) == 1 && current[0].ID == "a"
		}
		hostSeen = hostSeen || v["phase"] == "host" && len(current) == 0
	}
	last := events[len(events)-1]
	if last["capacity_available"] != r.Filesystems[0]["available"] {
		t.Fatalf("final progress did not refresh remaining capacity: %v / %v", last, r.Filesystems)
	}
	if !sharedSeen || !aliceSeen || !hostSeen || lastDone != 2 || lastBytes != r.Tree.Allocated || last["phase"] != "summarizing" {
		t.Fatalf("missing scan phase or final progress: shared=%v alice=%v host=%v last=%v", sharedSeen, aliceSeen, hostSeen, last)
	}
}

func TestVirtualFilesystemIsKeptForAccountingButExcludedFromCapacity(t *testing.T) {
	root := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Lstat(root, &st); err != nil {
		t.Fatal(err)
	}
	virtual := newScanner(defaultConfig(), []MountInfo{{Path: root, FS: "tmpfs"}}, nil)
	virtual.observeDevice(uint64(st.Dev), root)
	if len(virtual.capacities) != 0 || len(virtual.filesystems()) != 1 {
		t.Fatalf("virtual filesystem capacity leaked or accounting disappeared: %+v %+v", virtual.capacities, virtual.filesystems())
	}
	physical := newScanner(defaultConfig(), []MountInfo{{Path: root, FS: "ext4"}}, nil)
	physical.observeDevice(uint64(st.Dev), root)
	if len(physical.capacities) != 1 {
		t.Fatalf("physical filesystem capacity missing: %+v", physical.capacities)
	}
}

func TestProgressFinishesExcludedAndMissingContainerResources(t *testing.T) {
	root := t.TempDir()
	excluded, missing := filepath.Join(root, "excluded"), filepath.Join(root, "missing")
	if err := os.Mkdir(excluded, 0700); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Exclude = []string{excluded}
	var last object
	request := helperRequest{Config: c, Paths: []string{root, missing, root + "-missing"}, Mounts: []MountInfo{}, Containers: []scanContainer{{"a", "excluded"}, {"b", "missing"}, {"c", "no physical storage"}}, Resources: []Resource{
		{Path: filepath.Join(excluded, "unreachable", "upper"), Containers: []string{"a"}},
		{Path: filepath.Join(missing, "upper"), Containers: []string{"b"}},
		{Path: root + "-missing", Containers: []string{"b"}},
	}}
	r, err := scanPhysical(context.Background(), request, func(v object) error { last = v; return nil }, false)
	if err != nil || last["containers_remaining"] != 0 || last["containers_done"] != 3 || r.Tree.Errors == 0 || r.Tree.Excluded == 0 {
		t.Fatalf("inaccessible resources left containers pending: %v %v", err, last)
	}
}

func TestProgressCanCancelAtContainerBoundary(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "upper")
	if err := os.Mkdir(upper, 0700); err != nil {
		t.Fatal(err)
	}
	request := helperRequest{Config: defaultConfig(), Paths: []string{root}, Mounts: []MountInfo{}, Containers: []scanContainer{{"a", "alice"}}, Resources: []Resource{{Path: upper, Containers: []string{"a"}}}}
	_, err := scanPhysical(context.Background(), request, func(v object) error {
		if v["phase"] == "container" {
			return context.Canceled
		}
		return nil
	}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("container phase did not propagate cancellation: %v", err)
	}
}

func TestSnapshotSavingPreservesFinalProgress(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "data"), make([]byte, 4096))
	c := defaultConfig()
	c.Root, c.NoDocker = []string{root}, true
	var events []object
	r, err := buildSnapshot(context.Background(), c, func(v object) error { events = append(events, v); return nil })
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last["phase"] != "saving" || last["allocated"] != r.Tree.Allocated || last["capacity_known"] != true || last["containers_remaining"] != 0 || events[0]["phase"] != "host" {
		t.Fatalf("saving lost byte counts or mutated earlier reports: %v", last)
	}
}

func TestDiscoveryProgressReportsKnownContainerCount(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	var events []object
	_, _, _, _, err := discoverWithProgress(context.Background(), "team.user", 30, dockerFixture(t, true), func(v object) error { events = append(events, v); return nil })
	if err != nil {
		t.Fatal(err)
	}
	first, last := events[0], events[len(events)-1]
	if first["containers_total"] != nil || first["preparation_total"] != nil || first["preparation_done"] != 0 || last["containers_total"] != 1 || last["containers_discovered"] != 1 || last["containers_remaining"] != 1 || last["preparation_done"] != 7 || last["preparation_total"] != 7 {
		t.Fatalf("discovery confused discovered with scanned containers: %v", events)
	}
}

func TestDiscoveryProgressBeforeEachSlowContainerQuery(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	base := dockerFixture(t, true)
	var last object
	inspected, lastDone := 0, 0
	command := func(ctx context.Context, args []string, timeout int) (string, error) {
		if len(args) >= 2 && args[0] == "--host" {
			args = args[2:]
		}
		if args[0] == "ps" {
			if strings.Join(args, " ") != "ps -a --no-trunc --format {{.ID}}\t{{.Names}}" {
				t.Fatalf("container list must include names before size queries: %v", args)
			}
			return fmt.Sprintf("%s\tcontainer-1\n%s\tcontainer-2\n%s\tcontainer-3\n", ids[0], ids[1], ids[2]), nil
		}
		if args[0] != "container" {
			return base(ctx, args, timeout)
		}
		if len(args) != 4 || args[2] != "--size" || args[3] != ids[inspected] {
			t.Fatalf("size query must process one container at a time: %v", args)
		}
		current := last["current_containers"].([]scanContainer)
		if last["preparation_total"] != 9 || last["preparation_done"] != 4+inspected || last["containers_discovered"] != inspected || last["containers_done"] != 0 || last["allocated"] != nil || len(current) != 1 || current[0].ID != ids[inspected] {
			t.Fatalf("missing measured progress before blocking Docker query: %v", last)
		}
		name := fmt.Sprintf("container-%d", inspected+1)
		if current[0].Name != name || !strings.Contains(last["path"].(string), name) || strings.Contains(last["path"].(string), ids[inspected][:12]) {
			t.Fatalf("progress must show the container name before the size query: %v", last)
		}
		inspected++
		return fmt.Sprintf(`[{"Id":%q,"Name":"/container-%d","SizeRw":42}]`, args[3], inspected), nil
	}
	_, containers, resources, _, err := discoverWithProgress(context.Background(), "team.user", 30, command, func(v object) error {
		done := v["preparation_done"].(int)
		if done < lastDone {
			t.Fatalf("preparation count decreased: %v", v)
		}
		last, lastDone = v, done
		return nil
	})
	if err != nil || inspected != 3 || len(containers) != 3 || len(resources) != 1 || lastDone != 9 || last["containers_remaining"] != 3 || len(last["current_containers"].([]scanContainer)) != 0 {
		t.Fatalf("discovery did not finish containers and volumes: %v %v", err, last)
	}
	for _, c := range containers {
		if c.SizeRW == nil || *c.SizeRW != 42 {
			t.Fatal("per-container queries lost Docker logical size")
		}
	}
}

func TestDiscoveryProgressStopsBeforeNextContainer(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	for _, failure := range []error{context.Canceled, errors.New("progress write failed")} {
		base := dockerFixture(t, true)
		inspected := 0
		command := func(ctx context.Context, args []string, timeout int) (string, error) {
			if len(args) >= 2 && args[0] == "--host" {
				args = args[2:]
			}
			if args[0] == "ps" {
				return strings.Repeat("a", 64) + "\talice\n" + strings.Repeat("b", 64) + "\tbob", nil
			}
			if args[0] == "container" {
				inspected++
			}
			return base(ctx, args, timeout)
		}
		_, _, _, _, err := discoverWithProgress(context.Background(), "team.user", 30, command, func(v object) error {
			if v["containers_discovered"] == 1 {
				return failure
			}
			return nil
		})
		if !errors.Is(err, failure) || inspected != 1 {
			t.Fatalf("discovery continued after progress error: %d %v", inspected, err)
		}
	}
}

func TestDiscoveryProgressWithNoResources(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	base := dockerFixture(t, true)
	var last object
	_, _, _, _, err := discoverWithProgress(context.Background(), "team.user", 30, func(ctx context.Context, args []string, timeout int) (string, error) {
		if len(args) >= 2 && args[0] == "--host" {
			args = args[2:]
		}
		if args[0] == "ps" || args[0] == "volume" && args[1] == "ls" {
			return "", nil
		}
		return base(ctx, args, timeout)
	}, func(v object) error { last = v; return nil })
	if err != nil || last["preparation_done"] != 4 || last["preparation_total"] != 4 || last["containers_total"] != 0 {
		t.Fatalf("empty discovery failed to complete: %v %v", err, last)
	}
}
