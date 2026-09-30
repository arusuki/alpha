package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func scanForTest(t *testing.T, c Config, paths []string) *Node {
	t.Helper()
	n, err := newScanner(c, []MountInfo{}, nil).Scan(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestAccountingMatchesDU(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "models")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(nested, "weights")
	weights := []byte(strings.Repeat("x", 12345))
	mustWrite(t, f, weights)
	if err := os.Link(f, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(root, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	const sparseOffset = 32 * 1024 * 1024
	if _, err = sparse.WriteAt([]byte("x"), sparseOffset); err != nil {
		t.Fatal(err)
	}
	sparse.Close()
	tree := scanForTest(t, defaultConfig(), []string{root, nested, nested})
	raw, err := exec.Command("du", "-s", "-B1", root).Output()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := strconv.ParseInt(strings.Fields(string(raw))[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Allocated != expected {
		t.Fatalf("allocated: got %d, du=%d", tree.Allocated, expected)
	}
	// Count each inode once and exclude directory metadata from logical size.
	wantApparent := int64(len(weights)+len(nested)) + sparseOffset + 1
	if tree.Apparent != wantApparent {
		t.Fatalf("apparent: got %d, want %d", tree.Apparent, wantApparent)
	}
	if len(tree.Children) != 1 || tree.Apparent <= tree.Allocated+30*1024*1024 {
		t.Fatal("overlap or sparse accounting failed")
	}
	refs := 0
	for _, n := range snapshotNodes(tree) {
		if n.Kind == "reference" {
			refs++
		}
	}
	if refs != 1 || len(snapshotNodes(tree)[filepath.Join(root, "symlink")].Children) != 0 {
		t.Fatal("hardlink or symlink semantics failed")
	}
}
func TestDepthBudgetAndRequiredResources(t *testing.T) {
	root := t.TempDir()
	dir := root
	for i := 0; i < 12; i++ {
		dir = filepath.Join(dir, strconv.Itoa(i))
		os.Mkdir(dir, 0700)
		mustWrite(t, filepath.Join(dir, "file"), []byte(strings.Repeat("x", 2000)))
	}
	c := defaultConfig()
	c.MaxDepth = 50
	full := scanForTest(t, c, []string{root})
	c.MaxDepth = 0
	c.MaxNodes = 1
	small := scanForTest(t, c, []string{root})
	forced := scanForTest(t, c, []string{root, dir})
	if full.Allocated != small.Allocated || full.Files != small.Files || full.Allocated != forced.Allocated {
		t.Fatal("detail budget changed totals")
	}
	if small.Children[0].Omitted == 0 || snapshotNodes(forced)[dir] == nil {
		t.Fatal("resource path was dropped")
	}
}
func TestExcludesMountsAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"data", "merged", "skip"} {
		os.Mkdir(filepath.Join(root, n), 0700)
		mustWrite(t, filepath.Join(root, n, "x"), make([]byte, 4096))
	}
	c := defaultConfig()
	c.Exclude = []string{filepath.Join(root, "skip")}
	s := newScanner(c, []MountInfo{{Path: filepath.Join(root, "merged"), FS: "overlay"}}, nil)
	tree, err := s.Scan(context.Background(), []string{root, root + "-missing"})
	if err != nil {
		t.Fatal(err)
	}
	index := snapshotNodes(tree)
	if tree.Files != 1 || tree.Errors != 1 || index[filepath.Join(root, "skip")].Kind != "excluded" || index[filepath.Join(root, "merged")].Allocated != 0 || index[root+"-missing"].Kind != "unreadable" {
		t.Fatalf("invalid partial scan: %+v", tree)
	}
	if s.ErrorCount != 1 || len(s.Errors) != 1 {
		t.Fatal("missing warning")
	}
}
func TestSnapshotAtomicAndCancellation(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "x"), []byte("data"))
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{root}
	snapshot, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out.json")
	if err = atomicWrite(out, snapshot); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0600 || snapshot.Tree.Files != 1 || snapshot.SchemaVersion != snapshotVersion {
		t.Fatal("invalid snapshot")
	}
	raw, _ := os.ReadFile(out)
	if !json.Valid(raw) {
		t.Fatal("invalid result JSON")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = buildSnapshot(ctx, c, nil); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
}
func dockerFixture(t *testing.T, upper bool) dockerCommand {
	t.Helper()
	return func(ctx context.Context, args []string, timeout int) (string, error) {
		if len(args) >= 2 && args[0] == "--host" {
			if args[1] != "unix:///var/run/docker.sock" && args[1] != "unix:///run/user/1000/docker.sock" {
				t.Fatalf("discovery used the wrong local Docker endpoint: %v", args)
			}
			args = args[2:]
		}
		switch args[0] {
		case "context":
			return `[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`, nil
		case "info":
			return `{"ID":"fixture-daemon","DockerRootDir":"/docker","Driver":"overlay2","ServerVersion":"28"}`, nil
		case "ps":
			return strings.Repeat("a", 64) + "\talice", nil
		case "container":
			driver := `{}`
			if upper {
				driver = `{"Data":{"UpperDir":"/docker/upper"}}`
			}
			return fmt.Sprintf(`[{"Id":%q,"Name":"/alice","State":{"Status":"exited"},"Config":{"Image":"test","Labels":{"team.user":"alice"},"Env":["SECRET=do-not-export"]},"GraphDriver":%s,"SizeRw":42,"Mounts":[{"Type":"bind","Source":"/srv/models","Destination":"/models","RW":false}],"LogPath":"/docker/containers/a/a-json.log"}]`, strings.Repeat("a", 64), driver), nil
		case "volume":
			if args[1] == "ls" {
				return "orphan remote", nil
			}
			return `[{"Name":"orphan","Driver":"local","Mountpoint":"/docker/volumes/orphan/_data"},{"Name":"remote","Driver":"nfs"}]`, nil
		}
		return "", fmt.Errorf("unexpected Docker args %v", args)
	}
}
func TestDockerDiscovery(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	for _, upper := range []bool{true, false} {
		metadata, containers, resources, warnings, err := discoverWithProgress(context.Background(), "team.user", 30, dockerFixture(t, upper), nil)
		if err != nil {
			t.Fatal(err)
		}
		if metadata["endpoint"] != "unix:///var/run/docker.sock" || metadata["id"] != "fixture-daemon" || metadata["root"] != "/docker" || metadata["root_canonical"] != "/docker" {
			t.Fatalf("Docker identity was not persisted: %v", metadata)
		}
		c := containers[0]
		if c.Owner != "alice" || c.State != "exited" || c.SizeRW == nil || *c.SizeRW != 42 || strings.Contains(httpapi.JSONText(containers), "SECRET") {
			t.Fatal("container contract changed")
		}
		expected := 4
		wc := 1
		if !upper {
			expected = 3
			wc = 2
		}
		if len(resources) != expected || len(warnings) != wc || (c.UpperPath != nil) != upper {
			t.Fatal("resource discovery or fallback failed")
		}
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	metadata, _, _, _, err := discoverWithProgress(context.Background(), "team.user", 30, dockerFixture(t, true), nil)
	if err != nil || metadata["endpoint"] != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("custom local Docker endpoint was not preserved: %v %v", metadata, err)
	}
	t.Setenv("DOCKER_HOST", "ssh://server")
	if _, _, _, _, err := discoverWithProgress(context.Background(), "owner", 30, dockerFixture(t, true), nil); err == nil {
		t.Fatal("remote daemon accepted")
	}
}

func TestDockerSnapshotIdentityValidation(t *testing.T) {
	valid := object{"endpoint": "unix:///run/user/1000/docker.sock", "id": "daemon-01", "root": "/srv/docker", "root_canonical": "/srv/docker"}
	if err := validateSnapshotDocker(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []object{
		{"endpoint": "unix:///run/user/1000/docker.sock", "root": "/srv/docker", "root_canonical": "/srv/docker"},
		{"endpoint": "tcp://host:2375", "id": "daemon-01", "root": "/srv/docker", "root_canonical": "/srv/docker"},
		{"endpoint": "unix:///run/user/../docker.sock", "id": "daemon-01", "root": "/srv/docker", "root_canonical": "/srv/docker"},
		{"endpoint": "unix:///run/user/1000/docker.sock", "id": "daemon-01", "root": "../docker", "root_canonical": "/srv/docker"},
		{"endpoint": "unix:///run/user/1000/docker.sock", "id": "daemon-01", "root": "/srv/docker"},
	} {
		if err := validateSnapshotDocker(bad); err == nil {
			t.Fatalf("accepted invalid Docker snapshot identity: %v", bad)
		}
	}
}
func TestDockerCancellation(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "docker"), []byte("#!/bin/sh\nsleep 60\n"))
	os.Chmod(filepath.Join(dir, "docker"), 0700)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runDocker(ctx, []string{"info"}, 30); err == nil {
		t.Fatal("cancelled docker succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Docker cancellation left child running")
	}
}

func TestIncludeDockerRoot(t *testing.T) {
	storage := t.TempDir()
	mustWrite(t, filepath.Join(storage, "image-layer"), make([]byte, 4096))
	cli := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--host\" ]; then shift 2; fi\ncase \"$1\" in\ninfo) echo '" + httpapi.JSONText(object{"ID": "fixture-daemon", "DockerRootDir": storage, "Driver": "overlay2", "ServerVersion": "28"}) + "' ;;\nps|volume) ;;\n*) exit 1 ;;\nesac\n"
	mustWrite(t, filepath.Join(cli, "docker"), []byte(script))
	os.Chmod(filepath.Join(cli, "docker"), 0700)
	t.Setenv("PATH", cli+":"+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	c := defaultConfig()
	c.IncludeDockerRoot, c.ScanBackend = true, "host"
	result, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tree.Files != 1 || len(result.Resources) != 1 || result.Resources[0].Kinds[0] != "docker-root" || result.Docker["root"] != storage {
		t.Fatalf("Docker root was omitted: %+v", result)
	}
	c.IncludeDockerRoot = false
	result, err = buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tree.Files != 0 || len(result.Resources) != 0 {
		t.Fatal("Docker root scanned without opt-in")
	}
}

func TestFoldedReferencesAreReported(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "original"), make([]byte, 4096))
	if err := os.Link(filepath.Join(dir, "original"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{dir}
	c.MaxDepth = 0
	result, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scan["omitted_references"] != int64(1) || len(result.Warnings) != 1 {
		t.Fatalf("folded inode reference was not reported: %+v", result.Scan)
	}
}
