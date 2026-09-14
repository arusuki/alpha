package storage

import (
	"context"
	"fmt"
	"testing"
)

func mergeBenchmarkFixture(count int) (*Snapshot, *physicalScan, string) {
	path := "/data/target"
	target := &Node{Path: path, Kind: "directory", Children: []*Node{}}
	other := &Node{Path: "/unrelated", Kind: "directory", Children: []*Node{}}
	for i := range count {
		other.Children = append(other.Children, &Node{Path: fmt.Sprintf("/unrelated/%d", i), Kind: "directory", Children: []*Node{}})
	}
	root := &Node{Path: "@root", Kind: "root", Children: []*Node{target, other}}
	base := &Snapshot{SchemaVersion: snapshotVersion, Tree: root, Containers: []Container{}, Scan: object{}}
	branch := copyNode(target)
	for i := range 32 {
		branch.Children = append(branch.Children, &Node{Path: fmt.Sprintf("%s/%d", path, i), Kind: "directory", Children: []*Node{}})
	}
	return base, &physicalScan{Tree: &Node{Children: []*Node{branch}}, Accounting: []InodeRecord{}}, path
}

func BenchmarkSnapshotPrepare(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			base, physical, path := mergeBenchmarkFixture(count)
			merger, err := newDirectoryMerge(base, path)
			if err != nil {
				b.Fatal(err)
			}
			result, err := merger.merge(physical, false)
			if err != nil {
				b.Fatal(err)
			}
			writer := newSnapshotWriter(base, true)
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := writer.prepare(context.Background(), path, result); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Isolate publication work on a small selected branch of a large record.
// Baseline indexing is paid once per exploration, outside checkpoint timing.
func BenchmarkDirectoryMerge(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			base, physical, path := mergeBenchmarkFixture(count)
			merger, err := newDirectoryMerge(base, path)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := merger.merge(physical, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
