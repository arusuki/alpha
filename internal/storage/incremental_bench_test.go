package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Use the same filesystem tree and retained depth for both paths. Fixture
// creation is outside the timer; run with -bench IncrementalScan -benchmem.
func BenchmarkIncrementalScan(b *testing.B) {
	root := b.TempDir()
	for i := range 64 {
		dir := filepath.Join(root, fmt.Sprintf("dir-%03d", i), "nested")
		if err := os.MkdirAll(dir, 0700); err != nil {
			b.Fatal(err)
		}
		for j := range 256 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d.bin", j)), []byte("data"), 0600); err != nil {
				b.Fatal(err)
			}
		}
	}
	c := defaultConfig()
	c.NoDocker, c.ScanBackend, c.Root, c.MaxDepth, c.MaxNodes = true, "host", []string{root}, 3, maxScanNodes
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"local", "incremental", "published"} {
		b.Run(mode, func(b *testing.B) {
			db, err := openDatabase(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer db.SQL.Close()
			if _, err := db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES('base','completed','manual','test',1,'{}'),('worker','running','incremental','test',2,'{}')"); err != nil {
				b.Fatal(err)
			}
			current := base
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				if mode == "local" {
					_, err = buildSnapshot(context.Background(), c, nil)
				} else {
					var publish func(*Snapshot) error
					if mode == "published" {
						// A real exploration starts a new worker. Reuse its
						// cache across checkpoints, not across separate scans.
						publisher, err := db.newDirectoryPublisher("worker", scanPlan{BaseJobID: "base", IncrementalPath: root, BaseRevision: current.Revision}, current)
						if err != nil {
							b.Fatal(err)
						}
						publish = func(next *Snapshot) error {
							return publisher.publish(context.Background(), next, object{}, false)
						}
					}
					current, err = expandDirectory(context.Background(), current, c, root, nil, publish)
					if err == nil && publish != nil {
						err = publish(current)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(16513, "entries/op")
		})
	}
}

func BenchmarkIncrementalAdmission(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			db, err := openDatabase(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer db.SQL.Close()
			if _, err := db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES('base','completed','manual','test',1,'{}')"); err != nil {
				b.Fatal(err)
			}
			root := &Node{Path: "/data", Kind: "directory", Children: []*Node{}}
			for i := range count {
				root.Children = append(root.Children, &Node{Path: fmt.Sprintf("/data/%d", i), Kind: "directory", Children: []*Node{}})
			}
			base := &Snapshot{SchemaVersion: snapshotVersion, Tree: root}
			if err := db.Transaction(func(tx *sql.Tx) error { return storeSnapshot(tx, "base", root.Path, base, nil) }); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				if err := db.validateIncrementalRequest("base", "/data/0", 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
