package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type failingMountReader struct{}

func (failingMountReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestMountTableLongLineAndReadError(t *testing.T) {
	first := "1 0 8:1 / / rw - overlay overlay lowerdir=" + strings.Repeat("x", 70000) + "\n"
	second := "2 1 8:2 / /a\\040b rw - ext4 /dev/sdb rw"
	mounts, err := parseMountTable(strings.NewReader(first + second))
	if err != nil || len(mounts) != 2 || mounts[0].FS != "overlay" || mounts[1].Path != "/a b" {
		t.Fatalf("long mountinfo: %+v %v", mounts, err)
	}
	mounts, err = parseMountTable(io.MultiReader(strings.NewReader(first), failingMountReader{}))
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(mounts) != 1 {
		t.Fatalf("lost read error: %+v %v", mounts, err)
	}
}

func TestScanCLIRejectsInvalidConfigBeforeScanning(t *testing.T) {
	for _, args := range [][]string{
		{"--max-nodes", "1"}, {"--docker-timeout", "1"}, {"--max-depth", "33"}, {"--max-nodes", "100001"}, {"--docker-timeout", "3601"},
		{"--root", "relative"}, {"--exclude", "relative"}, {"--owner-label", "invalid label"}, {"--no-docker"},
	} {
		if err := ScanCLI(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
