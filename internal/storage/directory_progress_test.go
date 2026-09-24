package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func directoryProgressFixture(t *testing.T) (*Snapshot, Config, string) {
	t.Helper()
	root := t.TempDir()
	folded := filepath.Join(root, "folded")
	if err := os.Mkdir(folded, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		mustWrite(t, filepath.Join(folded, fmt.Sprintf("file-%04d", i)), make([]byte, 4096))
	}
	c := defaultConfig()
	c.NoDocker, c.Root, c.MaxDepth = true, []string{root}, 2
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshotNodes(base.Tree)[folded].Children) != 512 {
		t.Fatal("fixture must start with the deeper directory already expanded")
	}
	return base, c, root
}

func TestDirectoryProgressDoesNotCountFoldedHistoryTwice(t *testing.T) {
	base, c, root := directoryProgressFixture(t)
	// A Host report can rescan a parent at a shallower display depth. The
	// physical walk still measures those old children, even when folded.
	c.MaxDepth = 1
	updates := 0
	next, err := expandDirectory(context.Background(), base, c, root, nil, func(observation *Snapshot) error {
		updates++
		if observation.Tree.Allocated != base.Tree.Allocated || observation.Tree.Files != base.Tree.Files {
			return fmt.Errorf("unchanged filesystem grew during publication: bytes %d -> %d, files %d -> %d", base.Tree.Allocated, observation.Tree.Allocated, base.Tree.Files, observation.Tree.Files)
		}
		assertSnapshotAccounting(t, observation)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updates == 0 || next.Tree.Allocated != base.Tree.Allocated || next.Tree.Files != base.Tree.Files {
		t.Fatalf("missing updates or changed final accounting: updates=%d", updates)
	}
	assertSnapshotAccounting(t, next)
	if len(snapshotNodes(next.Tree)[filepath.Join(root, "folded")].Children) != 512 {
		t.Fatal("a shallower refresh discarded previously expanded detail")
	}
}

func TestDirectoryProgressPreservesRealChanges(t *testing.T) {
	for _, size := range []int{0, 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			base, c, root := directoryProgressFixture(t)
			file := filepath.Join(root, "folded", "file-0000")
			mustWrite(t, file, make([]byte, size))
			expected, err := buildSnapshot(context.Background(), c, nil)
			if err != nil {
				t.Fatal(err)
			}
			if expected.Tree.Allocated == base.Tree.Allocated {
				t.Fatal("fixture must change the allocated bytes")
			}
			c.MaxDepth = 1
			updates := 0
			next, err := expandDirectory(context.Background(), base, c, root, nil, func(observation *Snapshot) error {
				updates++
				// Until completion, unvisited historical bytes remain visible.
				// Real growth is allowed; recounting unchanged files is not.
				if observation.Tree.Allocated < min(base.Tree.Allocated, expected.Tree.Allocated) || observation.Tree.Allocated > max(base.Tree.Allocated, expected.Tree.Allocated) || observation.Tree.Files != base.Tree.Files {
					return fmt.Errorf("checkpoint inflated real change: before=%d current=%d final=%d", base.Tree.Allocated, observation.Tree.Allocated, expected.Tree.Allocated)
				}
				assertSnapshotAccounting(t, observation)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if updates == 0 || next.Tree.Allocated != expected.Tree.Allocated || snapshotNodes(next.Tree)[file].Allocated != snapshotNodes(expected.Tree)[file].Allocated {
				t.Fatal("refresh lost real file growth or shrinkage")
			}
			assertSnapshotAccounting(t, next)
		})
	}
}

func TestDirectoryProgressCancellationAndRetryDoNotInflateHistory(t *testing.T) {
	base, c, root := directoryProgressFixture(t)
	c.MaxDepth = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var checkpoint *Snapshot
	_, err := expandDirectory(ctx, base, c, root, nil, func(observation *Snapshot) error {
		checkpoint = observation
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || checkpoint == nil {
		t.Fatalf("expected cancellation after publication, got %v", err)
	}
	if checkpoint.Tree.Allocated != base.Tree.Allocated || checkpoint.Tree.Files != base.Tree.Files {
		t.Fatal("cancellation preserved an inflated intermediate total")
	}
	for i := 0; i < 2; i++ {
		checkpoint, err = expandDirectory(context.Background(), checkpoint, c, root, nil, func(observation *Snapshot) error {
			if observation.Tree.Allocated != base.Tree.Allocated || observation.Tree.Files != base.Tree.Files {
				return fmt.Errorf("retry counted historical bytes twice")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.Tree.Allocated != base.Tree.Allocated || checkpoint.Tree.Files != base.Tree.Files {
			t.Fatal("repeated refresh changed the final total")
		}
		assertSnapshotAccounting(t, checkpoint)
	}
}
