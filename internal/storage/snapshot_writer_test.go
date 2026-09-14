package storage

import (
	"context"
	"fmt"
	"testing"

	"project-alpha/internal/httpapi"
)

func TestSnapshotWriterRollbackRetryAndIdentityChanges(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err := db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES('base','completed','manual','test',1,'{}'),('worker','running','incremental','test',2,'{}')"); err != nil {
		t.Fatal(err)
	}
	base, physical, path := mergeBenchmarkFixture(300)
	branch := physical.Tree.Children[0]
	for i := len(branch.Children); i < 400; i++ {
		branch.Children = append(branch.Children, &Node{Path: fmt.Sprintf("%s/%d", path, i), Kind: "directory", Children: []*Node{}})
	}
	physical.Accounting = []InodeRecord{{Device: 1, Inode: 42, Path: branch.Children[0].Path}}
	merger, err := newDirectoryMerge(base, path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := merger.merge(physical, true)
	if err != nil {
		t.Fatal(err)
	}
	first.Revision = 1
	p, err := db.newDirectoryPublisher("worker", scanPlan{BaseJobID: "base", IncrementalPath: path}, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.publish(ctx, first, object{}, false); err != nil {
		t.Fatal(err)
	}
	// A detached, unchanged subtree with the same identities needs no SQL.
	unchanged, err := p.writer.prepare(ctx, path, first)
	if err != nil || len(unchanged.rows) != 0 || len(unchanged.removed) != 0 {
		t.Fatalf("unchanged observation generated writes: %v", err)
	}
	second := *first
	second.Revision++
	reduced := copyNode(branch)
	reduced.Children = reduced.Children[:17]
	reduced.Children[1] = copyNode(reduced.Children[1])
	reduced.Children[1].Name = "changed"
	second.Tree = applyDirectoryReplacement(first.Tree, path, reduced)
	if _, err := db.SQL.Exec("CREATE TRIGGER fail_commit BEFORE INSERT ON snapshot_changes BEGIN SELECT RAISE(ABORT,'reject commit'); END"); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(ctx, &second, object{}, false); err == nil {
		t.Fatal("publication should roll back after batch inserts and deletes")
	}
	if p.writer.previous != first || len(p.writer.nodes) != len(snapshotNodes(first.Tree)) {
		t.Fatal("failed commit advanced the cached index")
	}
	stored, err := db.readSnapshot("base")
	if err != nil || httpapi.JSONText(stored.Tree) != httpapi.JSONText(first.Tree) || stored.Revision != 1 {
		t.Fatalf("batch rollback changed the record: %v", err)
	}
	if _, err := db.SQL.Exec("DROP TRIGGER fail_commit"); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(ctx, &second, object{}, false); err != nil {
		t.Fatal("same publisher could not retry after rollback", err)
	}
	stored, err = db.readSnapshot("base")
	if err != nil || httpapi.JSONText(stored.Tree) != httpapi.JSONText(second.Tree) || len(p.writer.nodes) != len(snapshotNodes(second.Tree)) {
		t.Fatalf("batch deletion or retry corrupted cached/persisted nodes: %v", err)
	}
	// Identity changes cannot be skipped just because all Node pointers and
	// public byte counters are unchanged.
	third := second
	third.Revision++
	third.Accounting = append([]InodeRecord{}, second.Accounting...)
	third.Accounting[0].Inode++
	identityChange, err := p.writer.prepare(ctx, path, &third)
	if err != nil || len(identityChange.rows) != 1 {
		t.Fatalf("identity change did not produce exactly one changed row: %v", err)
	}
	if err := p.publish(ctx, &third, object{}, false); err != nil {
		t.Fatal(err)
	}
	stored, err = db.readSnapshot("base")
	if err != nil || len(stored.Accounting) != 1 || stored.Accounting[0] != third.Accounting[0] {
		t.Fatalf("identity cache hid a new inode: %v", err)
	}
}
