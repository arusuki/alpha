package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCleanupDeletionHandlesTemporaryIPCFiles(t *testing.T) {
	for _, kind := range []string{"fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "tmp")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(target, "ipc")
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				// Cleanup only needs the socket inode, not a listening endpoint.
				if err := unix.Mknod(path, unix.S_IFSOCK|0600, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(target, "data"), []byte("temporary data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := removeCleanupPath(context.Background(), target); err != nil {
				t.Fatalf("temporary %s prevented cleanup: %v", kind, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("target remains: %v", err)
			}
		})
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
	if err := removeCleanupPath(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("followed link", err)
	}
	parentLink := filepath.Join(root, "parent-link")
	os.Symlink(root, parentLink)
	if err := removeCleanupPath(context.Background(), filepath.Join(parentLink, "outside")); err == nil {
		t.Fatal("followed ancestor symlink")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeCleanupPath(ctx, outside); err == nil {
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
