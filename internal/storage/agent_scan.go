package storage

import (
	"container/heap"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type FileObservation struct {
	Path      string `json:"path"`
	Allocated int64  `json:"allocated"`
	Apparent  int64  `json:"apparent"`
	MTime     int64  `json:"mtime"`
	CTime     int64  `json:"ctime"`
}
type fileHeap []FileObservation

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return h[i].Allocated < h[j].Allocated }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(v any)        { *h = append(*h, v.(FileObservation)) }
func (h *fileHeap) Pop() any          { a := *h; v := a[len(a)-1]; *h = a[:len(a)-1]; return v }

type FileGroup struct {
	Name      string `json:"name"`
	Files     int64  `json:"files"`
	Allocated int64  `json:"allocated"`
	Apparent  int64  `json:"apparent"`
}
type FileAnalysis struct {
	Largest   []FileObservation `json:"largest_files"`
	Types     []FileGroup       `json:"file_types"`
	Modified  []FileGroup       `json:"modified_age"`
	TimeBasis string            `json:"time_basis"`
	top       fileHeap
	types     map[string]*FileGroup
	age       [3]FileGroup
	at        time.Time
}

func newFileAnalysis() *FileAnalysis {
	return &FileAnalysis{types: map[string]*FileGroup{}, at: time.Now(), age: [3]FileGroup{{Name: "within_90_days"}, {Name: "90_to_180_days"}, {Name: "over_180_days"}}}
}
func (a *FileAnalysis) observe(path string, st syscall.Stat_t) {
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return
	}
	f := FileObservation{path, st.Blocks * 512, st.Size, st.Mtim.Sec, st.Ctim.Sec}
	if a.top.Len() < 40 {
		heap.Push(&a.top, f)
	} else if f.Allocated > a.top[0].Allocated {
		a.top[0] = f
		heap.Fix(&a.top, 0)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		ext = "(no extension)"
	}
	if len(ext) > 32 {
		ext = "(other)"
	}
	if _, ok := a.types[ext]; !ok {
		if len(a.types) >= 256 {
			ext = "(other)"
		}
		if a.types[ext] == nil {
			a.types[ext] = &FileGroup{Name: ext}
		}
	}
	g := a.types[ext]
	g.Files++
	g.Allocated += f.Allocated
	g.Apparent += f.Apparent
	bucket := 0
	age := a.at.Unix() - f.MTime
	if age >= 180*86400 {
		bucket = 2
	} else if age >= 90*86400 {
		bucket = 1
	}
	g = &a.age[bucket]
	g.Files++
	g.Allocated += f.Allocated
	g.Apparent += f.Apparent
}
func (a *FileAnalysis) finish() {
	a.Largest = append([]FileObservation{}, a.top...)
	sort.Slice(a.Largest, func(i, j int) bool { return a.Largest[i].Allocated > a.Largest[j].Allocated })
	a.Types = []FileGroup{}
	for _, g := range a.types {
		a.Types = append(a.Types, *g)
	}
	sort.Slice(a.Types, func(i, j int) bool {
		if a.Types[i].Allocated == a.Types[j].Allocated {
			return a.Types[i].Name < a.Types[j].Name
		}
		return a.Types[i].Allocated > a.Types[j].Allocated
	})
	a.Modified = append([]FileGroup{}, a.age[:]...)
	a.TimeBasis = "文件 mtime 距扫描时刻的 90/180 天分桶；ctime 为最后状态变更，不是创建时间；这些时间不能证明最近读取或是否可删除。仅统计已读取且 inode 去重后的普通文件。"
}
func buildDetailSnapshot(ctx context.Context, c Config, path string, progress func(object) error) (*Snapshot, error) {
	progress = mergeScanProgress(progress)
	path, err := validateDetailPath(path, c, "")
	if err != nil {
		return nil, err
	}
	physical, err := InspectDirectories(ctx, c, DirectoryInspection{Paths: []string{path}, AnalyzeFiles: true}, progress)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if err := progress(object{"phase": "saving", "path": "扫描完成，正在保存结果"}); err != nil {
			return nil, err
		}
	}
	return &Snapshot{Accounting: physical.Accounting, SchemaVersion: snapshotVersion, FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Tree: physical.Tree, Docker: object{}, Resources: []Resource{{Path: path, Kinds: []string{"analysis"}, Containers: []string{}}}, Containers: []Container{}, Filesystems: physical.Filesystems, Warnings: physical.Warnings, Analysis: physical.Analysis, Scan: object{"scope": "agent-directory", "backend": physical.Backend, "visited_entries": physical.Visited, "omitted_references": physical.OmittedReferences, "error_count": physical.ErrorCount, "excludes": physical.Excludes}}, nil
}
