package storage

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBlockIdentityPartitionsAndBackingDisks(t *testing.T) {
	root := t.TempDir()
	mkdir := func(path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, path string) {
		t.Helper()
		mkdir(filepath.Dir(path))
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	disk := func(name string) string {
		path := filepath.Join(root, "devices", "pci", name)
		mkdir(filepath.Join(path, "slaves"))
		mkdir(filepath.Join(path, "device"))
		mustWrite(t, filepath.Join(path, "device", "model"), []byte("Example SSD\n"))
		return path
	}
	a, b := disk("nvme0n1"), disk("nvme1n1")
	for _, name := range []string{"nvme0n1p1", "nvme0n1p2"} {
		mkdir(filepath.Join(a, name))
		mustWrite(t, filepath.Join(a, name, "partition"), []byte("1"))
	}
	link(filepath.Join(a, "nvme0n1p1"), filepath.Join(root, "dev", "block", "259:1"))
	link(filepath.Join(a, "nvme0n1p2"), filepath.Join(root, "dev", "block", "259:2"))
	for i, want := range []string{"/dev/nvme0n1p1", "/dev/nvme0n1p2"} {
		device, disks := blockIdentity(root, unix.Mkdev(259, uint32(i+1)))
		if device != want || len(disks) != 1 || disks[0]["device"] != "/dev/nvme0n1" || disks[0]["model"] != "Example SSD" {
			t.Fatalf("partition identity: %s %v", device, disks)
		}
	}
	mapper := filepath.Join(root, "devices", "virtual", "block", "dm-0")
	link(a, filepath.Join(mapper, "slaves", "nvme0n1"))
	link(b, filepath.Join(mapper, "slaves", "nvme1n1"))
	link(mapper, filepath.Join(root, "dev", "block", "253:0"))
	device, disks := blockIdentity(root, unix.Mkdev(253, 0))
	if device != "/dev/dm-0" || len(disks) != 2 || disks[0]["device"] != "/dev/nvme0n1" || disks[1]["device"] != "/dev/nvme1n1" {
		t.Fatalf("mapped devices: %s %v", device, disks)
	}
	if device, disks := blockIdentity(root, unix.Mkdev(0, 99)); device != "" || len(disks) != 0 {
		t.Fatalf("invented device: %s %v", device, disks)
	}
}

func TestCapacityProgressKeepsFilesystemsIndependent(t *testing.T) {
	s := newScanner(defaultConfig(), []MountInfo{}, nil)
	s.capacities[1] = scanCapacity{100, 90, 5, object{"device": "1", "mount": "/"}}
	s.capacities[2] = scanCapacity{200, 20, 170, object{"device": "2", "mount": "/data"}}
	rows := s.capacityFilesystems()
	if len(rows) != 2 || rows[0]["mount"] != "/" || rows[0]["available"] != uint64(5) || rows[1]["mount"] != "/data" || rows[1]["available"] != uint64(170) {
		t.Fatalf("filesystem capacities pooled: %v", rows)
	}
}
