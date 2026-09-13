package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestOrdinaryScanStorageDoesNotGrowWithFoldedEntries(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "selected")
	boundary := filepath.Join(target, "folded")
	const count = 2000
	for i := 0; i < count; i++ {
		dir := filepath.Join(boundary, fmt.Sprintf("directory-%04d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "file.bin"), []byte("allocated data"))
	}
	c := defaultConfig()
	c.NoDocker, c.Root, c.MaxDepth, c.MaxNodes = true, []string{root}, 1, 2
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.Tree.Files != count || numberInt64(base.Scan["visited_entries"]) != 2*count+3 {
		t.Fatal("normal scan stopped accounting for folded descendants")
	}
	if base.Accounting != nil {
		t.Fatal("ordinary scan built an eager inode index")
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 16*1024 {
		t.Fatalf("folded entries inflated the snapshot: %d bytes", len(raw))
	}
	// The same physical result is sent through the Docker helper protocol.
	physical, err := scanPhysical(context.Background(), helperRequest{Config: c, Paths: c.Root}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if physical.Accounting != nil || len(httpapi.JSONText(helperEvent{Result: physical})) > 16*1024 {
		t.Fatal("helper transport still contains a global index")
	}
	// Directory inspections index retained nodes within the detail budget.
	physical, err = InspectDirectories(context.Background(), c, DirectoryInspection{Paths: c.Root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if physical.Accounting == nil || len(physical.Accounting) > c.MaxNodes {
		t.Fatal("inspection indexed hidden entries outside its detail budget")
	}
	next, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if numberInt64(next.Scan["last_incremental"].(object)["visited_entries"]) != 2*count+2 {
		t.Fatal("on-demand expansion did not measure folded descendants")
	}
	if next.Tree.Allocated != base.Tree.Allocated || next.Tree.Files != base.Tree.Files {
		t.Fatal("on-demand listing lost the historical folded totals")
	}
	if len(next.Accounting) != 2 {
		t.Fatal("on-demand index exceeded the observed scope")
	}
	t.Logf("ordinary scan: %d visited entries, %d bytes persisted; expansion measures descendants but retains only the next layer", 2*count+3, len(raw))
}

func TestIncrementalPreservesVisibleSharedClaimWithoutIndex(t *testing.T) {
	root := t.TempDir()
	target, other := filepath.Join(root, "a-selected"), filepath.Join(root, "b-outside")
	for _, dir := range []string{target, other} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	owner, alias := filepath.Join(target, "file.bin"), filepath.Join(other, "alias.bin")
	mustWrite(t, owner, make([]byte, 8192))
	if err := os.Link(owner, alias); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.NoDocker, c.Root = true, []string{target, other}
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.Accounting != nil || snapshotNodes(base.Tree)[alias].Reference != owner {
		t.Fatal("expected visible shared claim without an eager index")
	}
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, owner, make([]byte, 32768))
	next, err := expandForTest(context.Background(), base, c, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := snapshotNodes(next.Tree)[owner]
	if !n.SizeUnknown || n.Allocated != snapshotNodes(base.Tree)[owner].Allocated || next.Tree.Allocated != base.Tree.Allocated {
		t.Fatal("unverified replacement changed an outside shared claim")
	}
	assertSnapshotAccounting(t, next)
}
