package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/fsutil"
)

type Node struct {
	Scanning          bool             `json:"scanning,omitempty"`
	SizeUnknown       bool             `json:"size_unknown,omitempty"`
	DeviceAllocated   map[string]int64 `json:"device_allocated,omitempty"`
	Name              string           `json:"name"`
	Path              string           `json:"path"`
	Kind              string           `json:"kind"`
	Allocated         int64            `json:"allocated"`
	Apparent          int64            `json:"apparent"`
	Files             int64            `json:"files"`
	Errors            int64            `json:"errors"`
	Children          []*Node          `json:"children"`
	Omitted           int64            `json:"omitted_entries,omitempty"`
	Reference         string           `json:"reference,omitempty"`
	Reason            string           `json:"reason,omitempty"`
	Excluded          int64            `json:"excluded_entries,omitempty"`
	PermissionDenied  int64            `json:"permission_denied,omitempty"`
	OmittedReferences int64            `json:"omitted_references,omitempty"`
}
type Warning struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

const maxRetainedDetailNodes = 200000

type MountInfo struct{ Path, FS string }
type inode struct{ Dev, Ino uint64 }
type Scanner struct {
	StreamDirectory                       bool
	Ledger                                map[string]InodeRecord
	currentRecord                         InodeRecord
	Analysis                              *FileAnalysis
	Config                                Config
	Mounts                                []MountInfo
	Progress                              func(object) error
	ContainerProgress                     *containerProgress
	seen                                  map[inode]string
	required                              map[string]bool
	skip                                  map[string]bool
	devices                               map[uint64]string
	deviceAllocated                       map[uint64]int64
	capacities                            map[uint64]scanCapacity
	RequireReadOnly                       bool
	DetailRoots                           map[string]bool
	rootDevice                            uint64
	Nodes, Visited, ErrorCount, Allocated int64
	OmittedReferences                     int64
	Errors                                []Warning
	started, lastReport                   time.Time
}

func within(path, root string) bool {
	if path == root {
		return true
	}
	root = strings.TrimRight(root, "/")
	return len(path) > len(root) && path[len(root)] == '/' && strings.HasPrefix(path, root)
}

var mountEscape = regexp.MustCompile(`\\([0-7]{3})`)

func mountTable() []MountInfo {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		log.Printf("read mount table: %v", err)
		return []MountInfo{}
	}
	defer f.Close()
	mounts, err := parseMountTable(f)
	if err != nil {
		log.Printf("read mount table (incomplete): %v", err)
	}
	return mounts
}

func parseMountTable(r io.Reader) ([]MountInfo, error) {
	result := []MountInfo{}
	unescape := func(s string) string {
		return mountEscape.ReplaceAllStringFunc(s, func(s string) string { v, _ := strconv.ParseUint(s[1:], 8, 8); return string(rune(v)) })
	}
	// Overlay mount options can exceed Scanner's default 64 KiB token limit.
	lines := bufio.NewReader(r)
	for {
		line, err := lines.ReadString('\n')
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) == 2 {
			a, b := strings.Fields(parts[0]), strings.Fields(parts[1])
			if len(a) > 4 && len(b) > 1 {
				result = append(result, MountInfo{Path: unescape(a[4]), FS: b[0]})
			}
		}
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return result, err
		}
	}
}
func newScanner(c Config, mounts []MountInfo, progress func(object) error) *Scanner {
	if mounts == nil {
		mounts = mountTable()
	}
	c.Exclude = append([]string{}, c.Exclude...)
	for i, p := range c.Exclude {
		c.Exclude[i] = fsutil.Canonical(p)
	}
	s := &Scanner{Config: c, Mounts: mounts, Progress: progress, seen: map[inode]string{}, required: map[string]bool{}, skip: map[string]bool{}, devices: map[uint64]string{}, deviceAllocated: map[uint64]int64{}, Errors: []Warning{}, started: time.Now()}
	s.capacities = map[uint64]scanCapacity{}
	pseudo := " proc sysfs devtmpfs devpts tmpfs cgroup cgroup2 securityfs debugfs tracefs pstore mqueue hugetlbfs configfs fusectl autofs binfmt_misc rpc_pipefs nsfs overlay squashfs "
	for _, m := range mounts {
		if strings.Contains(pseudo, " "+m.FS+" ") {
			s.skip[m.Path] = true
		}
	}
	return s
}
func (s *Scanner) report(path string, force bool) error {
	if s.Progress != nil && (force || time.Since(s.lastReport) >= 500*time.Millisecond) {
		s.lastReport = time.Now()
		phase, backend := "host", "host"
		if s.Ledger != nil {
			phase = "directory"
		}
		if s.RequireReadOnly {
			backend = "docker"
		}
		var total, used, available uint64
		for _, capacity := range s.capacities {
			total += capacity.Total
			used += capacity.Used
			available += capacity.Available
		}
		v := object{"phase": phase, "backend": backend, "path": path, "entries": s.Visited, "allocated": s.Allocated, "errors": s.ErrorCount, "elapsed": float64(time.Since(s.started).Milliseconds()) / 1000, "capacity_total": total, "capacity_used": used, "capacity_available": available, "capacity_known": len(s.capacities) > 0}
		v["scan_mode"], v["gomaxprocs"] = s.Config.ScanMode, runtime.GOMAXPROCS(0)
		s.ContainerProgress.addTo(v, path)
		if path == "" {
			v["phase"], v["path"] = "summarizing", "汇总扫描结果与容器归属"
		}
		return s.Progress(v)
	}
	return nil
}
func (s *Scanner) recordError(path string, err error) {
	s.ErrorCount++
	if len(s.Errors) < 200 {
		s.Errors = append(s.Errors, Warning{path, err.Error()})
	}
}
func (s *Scanner) begin(path string, root bool) (*Node, *os.File) {
	s.currentRecord = InodeRecord{}
	s.Visited++
	n := &Node{Name: filepath.Base(path), Path: path, Kind: "file", Children: []*Node{}}
	for _, p := range s.Config.Exclude {
		if within(path, p) {
			n.Kind = "excluded"
			n.Reason = "excluded path or virtual/overlay mount"
			n.Excluded = 1
			return n, nil
		}
	}
	if !root && s.skip[path] {
		n.Kind = "excluded"
		n.Reason = "excluded path or virtual/overlay mount"
		n.Excluded = 1
		return n, nil
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		s.recordError(path, err)
		n.Kind = "unreadable"
		n.Errors = 1
		n.Reason = err.Error()
		if os.IsPermission(err) {
			n.PermissionDenied = 1
		}
		return n, nil
	}
	mode := st.Mode & syscall.S_IFMT
	if root {
		s.rootDevice = uint64(st.Dev)
	}
	if s.RequireReadOnly && (root || mode == syscall.S_IFDIR) {
		var fs syscall.Statfs_t
		err := syscall.Statfs(path, &fs)
		// Catch overlay mounts created while scanning, after mountinfo was read.
		if err == nil && !root && uint64(st.Dev) != s.rootDevice && fs.Type == 0x794c7630 {
			n.Kind, n.Reason, n.Excluded = "excluded", "nested overlay mount", 1
			return n, nil
		}
		if err != nil || fs.Flags&syscall.MS_RDONLY == 0 {
			if err == nil {
				err = fmt.Errorf("辅助扫描拒绝访问非只读挂载")
			}
			s.recordError(path, err)
			n.Kind, n.Reason, n.Errors = "unreadable", err.Error(), 1
			return n, nil
		}
	}
	key := inode{uint64(st.Dev), st.Ino}
	if s.Ledger != nil {
		s.currentRecord = InodeRecord{Device: key.Dev, Inode: key.Ino, Path: path}
	}
	if mode != syscall.S_IFLNK {
		s.observeDevice(key.Dev, path)
	}
	if ref, ok := s.seen[key]; ok && ref != path {
		n.Kind = "reference"
		n.Reference = ref
		return n, nil
	}
	if mode == syscall.S_IFDIR || st.Nlink > 1 {
		s.seen[key] = path
	}
	n.Allocated = st.Blocks * 512
	if s.Analysis != nil {
		s.Analysis.observe(path, st)
	}
	n.Apparent = st.Size
	s.Allocated += n.Allocated
	s.deviceAllocated[key.Dev] += n.Allocated
	n.DeviceAllocated = map[string]int64{strconv.FormatUint(key.Dev, 10): n.Allocated}
	if mode == syscall.S_IFDIR {
		n.Kind = "directory"
		// O_NOFOLLOW prevents a raced directory-to-symlink replacement from escaping the scan.
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			s.recordError(path, err)
			n.Errors = 1
			n.Reason = err.Error()
			if os.IsPermission(err) {
				n.PermissionDenied = 1
			}
			return n, nil
		}
		return n, os.NewFile(uintptr(fd), path)
	}
	n.Files = 1
	if mode == syscall.S_IFLNK {
		n.Kind = "symlink"
	}
	return n, nil
}
func aggregate(parent, child *Node) {
	if parent.DeviceAllocated == nil {
		parent.DeviceAllocated = map[string]int64{}
	}
	for dev, bytes := range child.DeviceAllocated {
		parent.DeviceAllocated[dev] += bytes
	}
	parent.Allocated += child.Allocated
	parent.Apparent += child.Apparent
	parent.Files += child.Files
	parent.Errors += child.Errors
	parent.Excluded += child.Excluded
	parent.PermissionDenied += child.PermissionDenied
	parent.OmittedReferences += child.OmittedReferences
}
func (s *Scanner) walk(ctx context.Context, path string) (*Node, error) {
	type frame struct {
		node        *Node
		record      InodeRecord
		dir         *os.File
		depth       int
		detailRoot  string
		detailDepth int
		keep        bool
		names       []string
	}
	root, dir := s.begin(path, true)
	s.Nodes++
	first := frame{node: root, record: s.currentRecord, dir: dir, keep: true}
	if s.DetailRoots[path] {
		first.detailRoot = path
	}
	// Reserve each relative depth separately: a large early branch must not
	// consume the slots needed to show later top-level directories (root/usr).
	detailLimits := [4]int{0, 128, 512, 1024}
	detailCounts := map[string][4]int{}
	stack := []frame{first}
	nextUpdate := time.Time{}
	update := func() error {
		if !s.StreamDirectory || s.Progress == nil || time.Now().Before(nextUpdate) {
			return nil
		}
		started := time.Now()
		// Stack frames contain completed children. Fold the in-flight branch
		// into a detached observation without modifying the scanner's totals.
		var branch *Node
		for i := len(stack) - 1; i >= 0; i-- {
			n := copyNode(stack[i].node)
			n.Scanning = true
			if branch != nil {
				aggregate(n, branch)
				if stack[i+1].keep {
					n.Children = append(n.Children, branch)
				} else {
					n.Omitted += 1 + branch.Omitted
				}
			}
			branch = n
		}
		err := s.Progress(object{"phase": "directory", "path": stack[len(stack)-1].node.Path, "entries": s.Visited, "allocated": s.Allocated, "directory_update": branch})
		// Start the interval after publication. Large observations can take
		// longer than the interval themselves; measuring from their start
		// would publish again immediately and starve filesystem traversal.
		// Reserve at least nine times the publication cost for useful work.
		nextUpdate = time.Now().Add(max(500*time.Millisecond, 9*time.Since(started)))
		return err
	}
	defer func() {
		for _, f := range stack {
			if f.dir != nil {
				f.dir.Close()
			}
		}
	}()
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := &stack[len(stack)-1]
		if f.dir != nil && len(f.names) == 0 {
			names, err := f.dir.Readdirnames(128)
			f.names = names
			if err != nil {
				if err != io.EOF {
					s.recordError(f.node.Path, err)
					f.node.Errors++
					f.node.Reason = err.Error()
					if os.IsPermission(err) {
						f.node.PermissionDenied++
					}
				}
				f.dir.Close()
				f.dir = nil
			}
		}
		if len(f.names) > 0 {
			name := f.names[0]
			f.names = f.names[1:]
			path := filepath.Join(f.node.Path, name)
			if s.ContainerProgress != nil && len(s.ContainerProgress.paths[path]) > 0 {
				if err := s.report(path, true); err != nil {
					return nil, err
				}
			}
			child, childDir := s.begin(path, false)
			detailRoot, detailDepth := f.detailRoot, f.detailDepth+1
			if s.DetailRoots[path] {
				detailRoot, detailDepth = path, 0
			}
			detailDirectory := detailRoot != "" && detailDepth > 0 && detailDepth <= 3 && child.Kind == "directory"
			counts := detailCounts[detailRoot]
			keepDetail := detailDirectory && counts[detailDepth] < detailLimits[detailDepth] && s.Nodes < maxRetainedDetailNodes
			keep := f.keep && (s.required[path] || keepDetail || (f.depth < s.Config.MaxDepth && s.Nodes < int64(s.Config.MaxNodes)))
			if keep {
				s.Nodes++
				if detailDirectory {
					counts[detailDepth]++
					detailCounts[detailRoot] = counts
				}
			}
			depth := f.depth + 1
			stack = append(stack, frame{node: child, record: s.currentRecord, dir: childDir, depth: depth, keep: keep, detailRoot: detailRoot, detailDepth: detailDepth})
			if s.Visited%256 == 0 {
				if err := update(); err != nil {
					return nil, err
				}
				if err := s.report(path, false); err != nil {
					return nil, err
				}
			}
			continue
		}
		completed := *f
		if completed.keep && completed.record.Path != "" {
			s.Ledger[completed.record.Path] = completed.record
		}
		stack = stack[:len(stack)-1]
		changed := s.ContainerProgress.finish(completed.node.Path)
		if changed && len(stack) > 0 {
			if err := s.report(stack[len(stack)-1].node.Path, true); err != nil {
				return nil, err
			}
		}
		if !completed.keep && completed.node.Kind == "reference" {
			s.OmittedReferences++
			completed.node.OmittedReferences++
		}
		if len(stack) > 0 {
			parent := stack[len(stack)-1].node
			aggregate(parent, completed.node)
			if completed.keep {
				parent.Children = append(parent.Children, completed.node)
			} else {
				parent.Omitted += 1 + completed.node.Omitted
			}
			if len(stack) == 1 {
				if err := update(); err != nil {
					return nil, err
				}
			}
		}
	}
	return root, nil
}
func (s *Scanner) Scan(ctx context.Context, paths []string) (*Node, error) {
	unique := map[string]bool{}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unique[fsutil.Canonical(p)] = true
		// Even an inaccessible resource belongs to a filesystem. Its nearest
		// accessible ancestor supplies capacity only, never additional scan bytes.
		s.probeCapacity(fsutil.Canonical(p))
	}
	paths = []string{}
	for p := range unique {
		paths = append(paths, p)
		for ancestor := p; ; ancestor = filepath.Dir(ancestor) {
			s.required[ancestor] = true
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if a != b {
			return a < b
		}
		return paths[i] < paths[j]
	})
	roots := []string{}
	for _, p := range paths {
		nested := false
		for _, r := range roots {
			if within(p, r) {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, p)
		}
	}
	// Include eligible nested filesystems up front so their capacity does not
	// appear only when a long traversal eventually reaches their mountpoint.
	for _, m := range s.Mounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		excluded := false
		for _, p := range s.Config.Exclude {
			if within(m.Path, p) {
				excluded = true
				break
			}
		}
		if excluded || s.skip[m.Path] {
			continue
		}
		for _, root := range roots {
			if within(m.Path, root) {
				reachable := true
				for ancestor := m.Path; ancestor != root; ancestor = filepath.Dir(ancestor) {
					if s.skip[ancestor] {
						reachable = false
						break
					}
				}
				if reachable {
					s.probeCapacity(m.Path)
				}
				break
			}
		}
	}
	tree := &Node{Name: "已扫描存储", Path: "@root", Kind: "root", Children: []*Node{}}
	for _, p := range roots {
		if err := s.report(p, true); err != nil {
			return nil, err
		}
		child, err := s.walk(ctx, p)
		if err != nil {
			return nil, err
		}
		tree.Children = append(tree.Children, child)
		aggregate(tree, child)
	}
	return tree, nil
}
func (s *Scanner) filesystems() []object {
	result := []object{}
	devices := make([]uint64, 0, len(s.devices))
	for dev := range s.devices {
		devices = append(devices, dev)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i] < devices[j] })
	for _, dev := range devices {
		path := s.devices[dev]
		var v syscall.Statfs_t
		if err := syscall.Statfs(path, &v); err != nil {
			s.recordError(path, fmt.Errorf("filesystem capacity: %w", err))
			continue
		}
		mount, fs := "", "unknown"
		for _, m := range s.Mounts {
			if within(path, m.Path) && len(m.Path) > len(mount) {
				mount = m.Path
				fs = m.FS
			}
		}
		if mount == "" {
			mount = path
		}
		block := v.Frsize
		if block == 0 {
			block = v.Bsize
		}
		b := uint64(block)
		used := int64((v.Blocks - v.Bfree) * b)
		scanned := s.deviceAllocated[dev]
		result = append(result, object{"device": strconv.FormatUint(dev, 10), "mount": mount, "fs": fs, "total": v.Blocks * b, "used": used, "available": v.Bavail * b, "reserved": (v.Bfree - v.Bavail) * b, "scanned": scanned, "unexplained": used - scanned})
	}
	return result
}

const snapshotVersion = 3

type Snapshot struct {
	Accounting        []InodeRecord                `json:"incremental_accounting,omitempty"`
	Revision          int64                        `json:"revision"`
	UpdatedAt         string                       `json:"updated_at,omitempty"`
	DirectoryAnalyses map[string]DirectoryAnalysis `json:"directory_analyses,omitempty"`
	SchemaVersion     int                          `json:"schema_version"`
	Host              string                       `json:"host"`
	FinishedAt        string                       `json:"finished_at"`
	Docker            object                       `json:"docker"`
	Tree              *Node                        `json:"tree"`
	Resources         []Resource                   `json:"resources"`
	Containers        []Container                  `json:"containers"`
	Filesystems       []object                     `json:"filesystems"`
	Warnings          []Warning                    `json:"warnings"`
	Scan              object                       `json:"scan"`
	JobID             string                       `json:"job_id,omitempty"`
}

func buildSnapshot(ctx context.Context, c Config, progress func(object) error) (*Snapshot, error) {
	progress = mergeScanProgress(progress)
	metadata := object{}
	containers := []Container{}
	resources := []Resource{}
	warnings := []Warning{}
	if !c.NoDocker {
		var err error
		metadata, containers, resources, warnings, err = discoverWithProgress(ctx, c.OwnerLabel, c.DockerTimeout, runDocker, progress)
		if err != nil {
			return nil, err
		}
	}
	for _, p := range c.Root {
		resources = append(resources, Resource{fsutil.Canonical(p), []string{"host"}, []string{}})
	}
	if p, _ := metadata["root"].(string); c.IncludeDockerRoot && p != "" {
		resources = append(resources, Resource{fsutil.Canonical(p), []string{"docker-root"}, []string{}})
	}
	if len(resources) == 0 {
		warnings = append(warnings, Warning{"", "No disk paths discovered. Use --root PATH to include host directories."})
	}
	paths := []string{}
	writablePaths := []string{}
	for _, row := range containers {
		if row.UpperPath != nil {
			writablePaths = append(writablePaths, *row.UpperPath)
		}
	}
	for _, r := range resources {
		paths = append(paths, r.Path)
	}
	targets := []scanContainer{}
	for _, row := range containers {
		targets = append(targets, scanContainer{ID: row.ID, Name: row.Name})
	}
	request := helperRequest{Version: 1, Config: c, Paths: paths, WritablePaths: writablePaths, Mounts: mountTable(), Containers: targets, Resources: resources}
	physical, err := collectRequest(ctx, request, progress)
	if err != nil {
		return nil, err
	}
	// Resolve resource aliases in the same host view that was actually scanned.
	for i := range resources {
		if p := physical.CanonicalPaths[resources[i].Path]; p != "" {
			resources[i].Path = p
		}
	}
	for i := range containers {
		row := &containers[i]
		if row.UpperPath != nil {
			if p := physical.CanonicalPaths[*row.UpperPath]; p != "" {
				row.UpperPath = &p
			}
		}
		for j := range row.Mounts {
			if row.Mounts[j].Source != nil {
				if p := physical.CanonicalPaths[*row.Mounts[j].Source]; p != "" {
					row.Mounts[j].Source = &p
				}
			}
		}
		if row.LogPath != nil {
			if p := physical.CanonicalPaths[fsutil.Canonical(filepath.Dir(*row.LogPath))]; p != "" {
				logPath := filepath.Join(p, filepath.Base(*row.LogPath))
				row.LogPath = &logPath
			}
		}
	}
	tree := physical.Tree
	host, _ := os.Hostname()
	layers, layerWarnings := writableLayers(containers, tree)
	warnings = append(warnings, layerWarnings...)
	warnings = append(warnings, physical.Warnings...)
	if physical.OmittedReferences > 0 {
		warnings = append(warnings, Warning{"", fmt.Sprintf("%d 个硬链接或目录别名引用被明细预算折叠；空间总量仍去重，容器和用户归属可能不完整。可增加明细深度与节点预算后重新扫描。", physical.OmittedReferences)})
	}
	if progress != nil {
		if err := progress(object{"phase": "saving", "path": "扫描完成，正在保存结果", "entries": physical.Visited, "allocated": physical.Tree.Allocated, "current_containers": []scanContainer{}}); err != nil {
			return nil, err
		}
	}
	return &Snapshot{Accounting: physical.Accounting, SchemaVersion: snapshotVersion, Host: host, FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Docker: metadata, Tree: tree, Resources: resources, Containers: containers, Filesystems: physical.Filesystems, Warnings: warnings, Scan: object{"max_depth": c.MaxDepth, "max_nodes": c.MaxNodes, "retained_nodes": physical.Nodes, "visited_entries": physical.Visited, "omitted_references": physical.OmittedReferences, "error_count": physical.ErrorCount, "excluded_entries": tree.Excluded, "permission_denied": tree.PermissionDenied, "euid": physical.EUID, "backend": physical.Backend, "writable_layers": layers, "excludes": physical.Excludes, "scope": "docker-resources-and-explicit-roots", "accounting": "st_blocks*512; inode dedup; symlinks not followed; nested roots counted once"}}, nil
}
func atomicWrite(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".scan-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync result directory: %w", err)
	}
	return nil
}
