package storage

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCleanupPreservesSocketAndContainingDirectories(t *testing.T) {
	target := filepath.Join(t.TempDir(), "tmp")
	nested := filepath.Join(target, "tmux", "user")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(nested, "default")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, p := range []string{filepath.Join(target, "data"), filepath.Join(nested, "data")} {
		if err := os.WriteFile(p, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(target, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	stats := new(cleanupStats)
	if err := removeCleanupPath(context.Background(), target, stats); err != nil {
		t.Fatal(err)
	}
	if stats.Sockets != 1 || stats.CharDevices != 0 {
		t.Fatal(stats)
	}
	for _, p := range []string{filepath.Join(target, "data"), filepath.Join(nested, "data"), filepath.Join(target, "empty")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("ordinary content remains", p, err)
		}
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal("socket no longer connectable", err)
	}
	conn.Close()
	if err := os.WriteFile(filepath.Join(nested, "new"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupSkipsCharDevicesAndRemovesFIFO(t *testing.T) {
	for _, kind := range []string{"fifo", "char"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "cache")
			nested := filepath.Join(target, "child")
			if err := os.MkdirAll(nested, 0700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(nested, "special")
			if kind == "fifo" {
				if err := unix.Mkfifo(p, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := unix.Mknod(p, unix.S_IFCHR|0600, int(unix.Mkdev(1, 3))); err != nil {
					if err == unix.EPERM {
						t.Skip("requires CAP_MKNOD; covered by Docker test")
					}
					t.Fatal(err)
				}
			}
			stats := new(cleanupStats)
			if err := removeCleanupPath(context.Background(), target, stats); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(p)
			if kind == "char" {
				if err != nil || stats.CharDevices != 1 {
					t.Fatal(stats, err)
				}
			} else if !os.IsNotExist(err) || stats.CharDevices != 0 {
				t.Fatal(stats, err)
			}
		})
	}
}

func TestCleanupPreservesDirectoryIdentityPermissionsAndOpenFD(t *testing.T) {
	target := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := os.Mkdir(filepath.Join(target, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "child", "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := removeCleanupPath(context.Background(), target, new(cleanupStats)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("directory identity or permissions changed: %v -> %v", before, after)
	}
	child, err := unix.Openat(int(fd.Fd()), "new-file", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("preexisting directory descriptor cannot create files", err)
	}
	unix.Close(child)
	if _, err := os.Stat(filepath.Join(target, "new-file")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupDeletionDoesNotFollowLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	target := filepath.Join(root, "target")
	for _, p := range []string{outside, target} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "link")); err != nil {
		t.Fatal(err)
	}
	if err := removeCleanupPath(context.Background(), target, new(cleanupStats)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("followed link", err)
	}
	parentLink := filepath.Join(root, "parent-link")
	os.Symlink(root, parentLink)
	if err := removeCleanupPath(context.Background(), filepath.Join(parentLink, "outside"), new(cleanupStats)); err == nil {
		t.Fatal("followed ancestor symlink")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeCleanupPath(ctx, outside, new(cleanupStats)); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestCleanupProtectsRootsMountsAndExclusions(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "platform")
	safe := filepath.Join(root, "cache")
	paths := []string{"/", "/etc", "/etc/ssl", "/proc/1", root, data, filepath.Join(data, "nested"), filepath.Join(root, "mounted"), filepath.Join(root, "excluded"), filepath.Join(root, "upper"), safe}
	tree := &Node{Path: "@root", Kind: "virtual"}
	tree.Children = []*Node{{Path: safe, Kind: "directory"}}
	upper := filepath.Join(root, "upper")
	snapshot := &Snapshot{Tree: tree, Scan: object{}, Containers: []Container{{UpperPath: &upper}}}
	mounts := []MountInfo{{Path: filepath.Join(root, "mounted", "inner")}}
	for _, p := range paths[:len(paths)-1] {
		tree.Children = []*Node{{Path: p, Kind: "directory"}}
		if err := validateCleanupPath(snapshot, data, []string{filepath.Join(root, "excluded")}, mounts, p); err == nil {
			t.Fatalf("accepted protected path %s", p)
		}
	}
	tree.Children = []*Node{{Path: safe, Kind: "directory"}}
	if err := validateCleanupPath(snapshot, data, nil, mounts, safe); err != nil {
		t.Fatal(err)
	}
	if err := validateCleanupPath(snapshot, data, nil, mounts, filepath.Join(root, "not-in-snapshot")); err == nil {
		t.Fatal("accepted unrecorded path")
	}
}
