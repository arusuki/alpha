package storage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestWritableLayersAndWholeDiskDoNotDoubleCount(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "overlay2", "layer", "diff")
	if err := os.MkdirAll(upper, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(upper, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("x"), 32*1024*1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	mustWrite(t, filepath.Join(root, "image"), make([]byte, 8192))
	c := defaultConfig()
	c.MaxDepth, c.MaxNodes = 0, 1
	s := newScanner(c, []MountInfo{}, nil)
	tree, err := s.Scan(context.Background(), []string{root, upper, upper})
	if err != nil {
		t.Fatal(err)
	}
	logical := int64(100 * 1024 * 1024)
	containers := []Container{{ID: "running", State: "running", UpperPath: &upper, SizeRW: &logical}, {ID: "stopped", State: "exited"}}
	summary, warnings := writableLayers(containers, tree)
	w := containers[0].WritableLayer
	for _, apparent := range []bool{false, true} {
		args := []string{"-s", "-B1"}
		if apparent {
			args = append(args, "--apparent-size")
		}
		raw, err := exec.Command("du", append(args, upper)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := strconv.ParseInt(strings.Fields(string(raw))[0], 10, 64)
		got := w.Allocated
		if apparent {
			got = w.Apparent
		}
		if got == nil || *got != want {
			t.Fatalf("apparent %v: %+v, du %d", apparent, w, want)
		}
	}
	if w.Status != "complete" || summary["complete"] != 1 || summary["unknown"] != 1 || len(warnings) != 1 || containers[1].WritableLayer.Allocated != nil {
		t.Fatalf("invalid layers: %+v %+v", w, summary)
	}
	raw, err := exec.Command("du", "-s", "-B1", root).Output()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := strconv.ParseInt(strings.Fields(string(raw))[0], 10, 64)
	if tree.Allocated != want {
		t.Fatalf("writable layer counted twice: %d != %d", tree.Allocated, want)
	}
	disks := s.filesystems()
	if len(disks) != 1 || disks[0]["scanned"] != tree.Allocated || disks[0]["unexplained"].(int64)+tree.Allocated != disks[0]["used"].(int64) {
		t.Fatalf("invalid disk reconciliation: %+v", disks)
	}
}

func TestWritableLayerMissingExcludedAndPermissionDenied(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "upper")
	os.Mkdir(upper, 0700)
	mustWrite(t, filepath.Join(upper, "keep"), make([]byte, 4096))
	excluded := filepath.Join(upper, "skip")
	mustWrite(t, excluded, make([]byte, 4096))
	c := defaultConfig()
	c.MaxDepth, c.MaxNodes, c.Exclude = 0, 1, []string{excluded}
	tree := scanForTest(t, c, []string{root, upper})
	containers := []Container{{UpperPath: &upper}}
	writableLayers(containers, tree)
	w := containers[0].WritableLayer
	if w.Status != "partial" || w.Allocated == nil || tree.Excluded != 1 || snapshotNodes(tree)[excluded] != nil {
		t.Fatalf("folded exclusion lost: %+v %+v", tree, w)
	}
	missing := filepath.Join(root, "missing")
	containers[0].UpperPath = &missing
	writableLayers(containers, scanForTest(t, c, []string{missing}))
	if containers[0].WritableLayer.Status != "unreadable" || containers[0].WritableLayer.Allocated != nil {
		t.Fatal("missing upper became zero")
	}
	// An inaccessible parent can hide UpperDir when whole-root scanning is enabled.
	parent := &Node{Path: root, Kind: "unreadable", Errors: 1, PermissionDenied: 1, Reason: "permission denied", Children: []*Node{}}
	containers[0].UpperPath = &upper
	summary, warnings := writableLayers(containers, parent)
	w = containers[0].WritableLayer
	if w.Status != "unreadable" || w.Allocated != nil || !w.PermissionDenied || summary["permission_denied"] != 1 || len(warnings) != 1 || !strings.Contains(w.Reason, "Docker") {
		t.Fatalf("permission failure hidden: %+v %+v", w, summary)
	}
	parent.Kind, parent.Reason, parent.PermissionDenied = "excluded", "excluded", 0
	writableLayers(containers, parent)
	if containers[0].WritableLayer.Status != "excluded" {
		t.Fatal("excluded ancestor hidden")
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(upper, 0000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(upper, 0700)
		tree = scanForTest(t, defaultConfig(), []string{upper})
		writableLayers(containers, tree)
		if !containers[0].WritableLayer.PermissionDenied || tree.PermissionDenied != 1 {
			t.Fatal("real permission denial not detected")
		}
	}
}

func TestFilesystemReconciliationUsesEachDeviceAndPreservesNegativeGap(t *testing.T) {
	dir := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Lstat(dir, &st); err != nil {
		t.Fatal(err)
	}
	s := newScanner(defaultConfig(), []MountInfo{}, nil)
	s.devices[uint64(st.Dev)] = dir
	s.deviceAllocated[uint64(st.Dev)] = 1 << 62
	disks := s.filesystems()
	if disks[0]["unexplained"].(int64) >= 0 || disks[0]["scanned"] != int64(1<<62) {
		t.Fatalf("negative gap hidden: %+v", disks)
	}
	// /dev/shm provides a second real device without requiring mount privileges.
	other, err := os.MkdirTemp("/dev/shm", "project-alpha-accounting-")
	if err != nil {
		t.Skip("second writable filesystem unavailable")
	}
	defer os.RemoveAll(other)
	var otherStat syscall.Stat_t
	if err := syscall.Lstat(other, &otherStat); err != nil {
		t.Fatal(err)
	}
	if st.Dev == otherStat.Dev {
		t.Skip("same filesystem")
	}
	mustWrite(t, filepath.Join(dir, "a"), make([]byte, 4096))
	mustWrite(t, filepath.Join(other, "b"), make([]byte, 16384))
	s = newScanner(defaultConfig(), []MountInfo{}, nil)
	tree, err := s.Scan(context.Background(), []string{dir, other})
	if err != nil {
		t.Fatal(err)
	}
	disks = s.filesystems()
	if len(disks) != 2 {
		t.Fatalf("devices merged: %+v", disks)
	}
	var sum int64
	for _, d := range disks {
		dev, _ := strconv.ParseUint(d["device"].(string), 10, 64)
		want := int64(0)
		for _, n := range tree.Children {
			var ns syscall.Stat_t
			if err := syscall.Lstat(n.Path, &ns); err != nil {
				t.Fatal(err)
			}
			if uint64(ns.Dev) == dev {
				want += n.Allocated
			}
		}
		if d["scanned"] != want {
			t.Fatalf("charged bytes from another device: %+v want %d", d, want)
		}
		sum += d["scanned"].(int64)
	}
	if sum != tree.Allocated {
		t.Fatal("device totals not conserved")
	}
}

func TestWritableLayerExternalHardlinksStayDeduplicated(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	mustWrite(t, filepath.Join(a, "file"), make([]byte, 8192))
	if err := os.Link(filepath.Join(a, "file"), filepath.Join(b, "link")); err != nil {
		t.Fatal(err)
	}
	tree := scanForTest(t, defaultConfig(), []string{a, b})
	containers := []Container{{UpperPath: &a}, {UpperPath: &b}}
	writableLayers(containers, tree)
	if containers[1].WritableLayer.Status != "partial" {
		t.Fatal("external hard link reported as complete layer")
	}
	if *containers[0].WritableLayer.Allocated+*containers[1].WritableLayer.Allocated != tree.Allocated {
		t.Fatal("layer summary duplicated shared inode")
	}
}

func TestScanCLIWritesWritableLayerAndDiskReconciliation(t *testing.T) {
	root, cli := t.TempDir(), t.TempDir()
	upper := filepath.Join(root, "upper")
	if err := os.Mkdir(upper, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(upper, "data"), make([]byte, 8192))
	records := []object{{"Id": strings.Repeat("a", 64), "Name": "/stopped", "State": object{"Status": "exited"}, "GraphDriver": object{"Data": object{"UpperDir": upper}}, "SizeRw": 999999}}
	script := "#!/bin/sh\nif [ \"$1\" = \"--host\" ]; then shift 2; fi\ncase \"$1\" in\ninfo) echo '" + httpapi.JSONText(object{"ID": "fixture-daemon", "DockerRootDir": root}) + "' ;;\nps) echo '" + strings.Repeat("a", 64) + "\tstopped' ;;\ncontainer) echo '" + httpapi.JSONText(records) + "' ;;\nvolume) ;;\n*) exit 1 ;;\nesac\n"
	mustWrite(t, filepath.Join(cli, "docker"), []byte(script))
	if err := os.Chmod(filepath.Join(cli, "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+":"+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	output := filepath.Join(t.TempDir(), "snapshot.json")
	if err := ScanCLI(context.Background(), []string{"--include-docker-root", "--root", root, "--max-depth", "0", "--max-nodes", "100", "--output", output}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	c := snapshot.Containers[0]
	if c.State != "exited" || c.WritableLayer == nil || c.WritableLayer.Status != "complete" || c.WritableLayer.Allocated == nil || *c.WritableLayer.Allocated != snapshot.Tree.Children[0].Children[0].Allocated || c.SizeRW == nil || *c.SizeRW != 999999 {
		t.Fatalf("invalid persisted layer: %+v", c)
	}
	disk := snapshot.Filesystems[0]
	if disk["scanned"] != float64(snapshot.Tree.Allocated) || disk["unexplained"].(float64)+disk["scanned"].(float64) != disk["used"].(float64) {
		t.Fatalf("invalid persisted reconciliation: %+v", disk)
	}
	if snapshot.Scan["writable_layers"].(map[string]any)["complete"] != float64(1) {
		t.Fatal("missing layer audit")
	}
}
