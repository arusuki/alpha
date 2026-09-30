package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestApparentSizeExcludesDirectoryMetadata(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}

	tree := scanForTest(t, defaultConfig(), []string{root})
	if tree.Apparent != 0 {
		t.Fatalf("empty directories have apparent size %d, want 0", tree.Apparent)
	}
	var allocated int64
	for _, path := range []string{root, nested} {
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			t.Fatal(err)
		}
		allocated += st.Blocks * 512
	}
	if tree.Allocated != allocated {
		t.Fatalf("directory allocation = %d, want %d", tree.Allocated, allocated)
	}

	file := filepath.Join(nested, "file")
	mustWrite(t, file, []byte("payload"))
	if err := os.Link(file, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	// The link's own length counts; its target must not be traversed.
	const target = "nested"
	if err := os.Symlink(target, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	tree = scanForTest(t, defaultConfig(), []string{root})
	want := int64(len("payload") + len(target))
	if tree.Apparent != want {
		t.Fatalf("apparent size = %d, want %d (one file plus symlink)", tree.Apparent, want)
	}
}
