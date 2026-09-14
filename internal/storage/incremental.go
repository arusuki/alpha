package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
)

// Private identities for retained inspection nodes, used to deduplicate links.
type InodeRecord struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Path   string `json:"path"`
}

func snapshotNodes(tree *Node) map[string]*Node {
	nodes := map[string]*Node{}
	pending := []*Node{tree}
	for len(pending) > 0 {
		n := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if n == nil {
			continue
		}
		nodes[n.Path] = n
		pending = append(pending, n.Children...)
	}
	return nodes
}
func validateIncrementalTarget(base *Snapshot, path string) error {
	if base == nil || base.Tree == nil {
		return httpapi.NewError(409, "该扫描记录不支持继续分析")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || len(path) > 4096 {
		return httpapi.NewError(400, "目录路径无效")
	}
	n := snapshotNodes(base.Tree)[path]
	if n == nil || n.Kind != "directory" {
		return httpapi.NewError(400, "只能继续分析扫描记录中已有的物理目录")
	}
	return nil
}

const (
	incrementalDepth    = 1
	maxIncrementalDepth = 3
	maxSnapshotNodes    = 250000
)

func validateIncrementalDepth(depth int) error {
	if depth < 1 || depth > maxIncrementalDepth {
		return httpapi.NewError(400, "每次分析深度必须为 1–3 层")
	}
	return nil
}

// Depth limits retained display detail, never the recursive byte measurement.
func expandDirectory(ctx context.Context, base *Snapshot, c Config, path string, progress func(object) error, publish func(*Snapshot) error) (*Snapshot, error) {
	if err := validateIncrementalTarget(base, path); err != nil {
		return nil, err
	}
	if err := validateIncrementalDepth(c.MaxDepth); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch excludes := base.Scan["excludes"].(type) {
	case []string:
		c.Exclude = append(append([]string{}, c.Exclude...), excludes...)
	case []any:
		c.Exclude = append([]string{}, c.Exclude...)
		for _, v := range excludes {
			if p, ok := v.(string); ok {
				c.Exclude = append(c.Exclude, p)
			}
		}
	}
	c.MaxNodes = maxScanNodes
	if backend, _ := base.Scan["backend"].(string); backend == "docker" {
		c.ScanBackend, c.NoDocker = "docker", false
	}
	inspection := DirectoryInspection{Paths: []string{path}, AnalyzeFiles: true, StreamDirectory: publish != nil}
	baseNodes := snapshotNodes(base.Tree)
	for _, n := range baseNodes {
		if !within(n.Path, path) && within(n.Reference, path) {
			inspection.RetainPaths = append(inspection.RetainPaths, n.Reference)
		}
	}
	for _, r := range base.Accounting {
		if n := baseNodes[r.Path]; n != nil && n.Kind != "reference" && !within(r.Path, path) {
			inspection.Seed = append(inspection.Seed, r)
		}
	}
	report := mergeScanProgress(progress)
	revision := base.Revision
	physical, err := InspectDirectories(ctx, c, inspection, func(v object) error {
		if value := v["directory_update"]; value != nil {
			node, ok := value.(*Node)
			if !ok {
				node = &Node{}
				if err := json.Unmarshal([]byte(httpapi.JSONText(value)), node); err != nil {
					return err
				}
			}
			delete(v, "directory_update")
			if publish == nil || node.Path != path || node.Kind != "directory" {
				return fmt.Errorf("目录更新与请求不一致")
			}
			partial := &physicalScan{Tree: &Node{Children: []*Node{node}}, Backend: c.ScanBackend, Visited: numberInt64(v["entries"])}
			next, err := mergeDirectoryResult(base, partial, path, false)
			if err != nil {
				return err
			}
			next.Revision = revision + 1
			if err = publish(next); err != nil {
				return err
			}
			revision = next.Revision
			v["record_allocated"] = next.Tree.Allocated
		}
		if report != nil {
			return report(v)
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	if physical.Accounting == nil || len(physical.Tree.Children) != 1 || physical.Tree.Children[0].Path != path {
		return nil, fmt.Errorf("目录检查结果与请求不一致")
	}
	raw := physical.Tree.Children[0]
	if raw.Kind != "directory" || raw.Reason != "" {
		return nil, fmt.Errorf("目录未完整读取：%s（%s）", path, raw.Reason)
	}
	result, err := mergeDirectoryResult(base, physical, path, true)
	if err != nil {
		return nil, err
	}
	result.Revision = revision + 1
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func mergeDirectoryResult(base *Snapshot, physical *physicalScan, path string, complete bool) (*Snapshot, error) {
	result := *base
	result.Containers = append([]Container{}, base.Containers...)
	result.Scan = maps.Clone(base.Scan)
	rawRoot := physical.Tree.Children[0]
	if complete {
		rawRoot = preserveSharedClaims(base, physical, path)
	}
	replacement := rawRoot
	if !complete {
		replacement = mergeDirectoryObservation(snapshotNodes(base.Tree)[path], rawRoot)
	}
	result.Tree = applyDirectoryReplacement(result.Tree, path, replacement)
	nodes := snapshotNodes(result.Tree)
	if len(nodes) > maxSnapshotNodes {
		return nil, fmt.Errorf("补充后超过 25 万明细节点，请发起新扫描")
	}
	records := []InodeRecord{}
	for _, r := range base.Accounting {
		if !complete || !within(r.Path, path) || (nodes[r.Path] != nil && nodes[r.Path].SizeUnknown) {
			records = append(records, r)
		}
	}
	for _, r := range physical.Accounting {
		if nodes[r.Path] != nil && !nodes[r.Path].SizeUnknown {
			records = append(records, r)
		}
	}
	result.Accounting = records
	fsByDevice := map[string]object{}
	for _, fs := range base.Filesystems {
		fsByDevice[fmt.Sprint(fs["device"])] = maps.Clone(fs)
	}
	for _, fs := range physical.Filesystems {
		fsByDevice[fmt.Sprint(fs["device"])] = maps.Clone(fs)
	}
	deviceKnown := validDeviceTotal(result.Tree)
	if deviceKnown {
		for dev, bytes := range result.Tree.DeviceAllocated {
			if bytes > 0 && fsByDevice[dev] == nil {
				deviceKnown = false
				break
			}
		}
	}
	result.Filesystems = []object{}
	for dev, fs := range fsByDevice {
		if deviceKnown {
			fs["scanned"] = result.Tree.DeviceAllocated[dev]
			fs["unexplained"] = numberInt64(fs["used"]) - result.Tree.DeviceAllocated[dev]
		} else {
			fs["scanned"] = nil
			fs["unexplained"] = nil
		}
		result.Filesystems = append(result.Filesystems, fs)
	}
	sort.Slice(result.Filesystems, func(i, j int) bool {
		return fmt.Sprint(result.Filesystems[i]["device"]) < fmt.Sprint(result.Filesystems[j]["device"])
	})
	warnings := []Warning{}
	observedNodes := snapshotNodes(rawRoot)
	for _, w := range result.Warnings {
		if strings.HasPrefix(w.Message, "可写层：") {
			continue
		}
		if observedNodes[w.Path] != nil {
			continue
		}
		warnings = append(warnings, w)
	}
	warnings = append(warnings, physical.Warnings...)
	layers, layerWarnings := writableLayers(result.Containers, result.Tree)
	result.Warnings = append(warnings, layerWarnings...)
	if result.Scan == nil {
		result.Scan = object{}
	}
	result.Scan["retained_nodes"] = len(nodes) - 1
	result.Scan["omitted_references"] = result.Tree.OmittedReferences
	result.Scan["error_count"] = result.Tree.Errors
	result.Scan["excluded_entries"] = result.Tree.Excluded
	result.Scan["permission_denied"] = result.Tree.PermissionDenied
	result.Scan["writable_layers"] = layers
	limited := !deviceKnown || result.Tree.OmittedReferences > 0
	for _, n := range nodes {
		limited = limited || n.Scanning || n.SizeUnknown
	}
	result.Scan["lazy_accounting_limited"] = limited
	result.Scan["last_incremental"] = object{"path": path, "backend": physical.Backend, "visited_entries": physical.Visited, "allocated_delta": result.Tree.Allocated - base.Tree.Allocated, "complete": complete}
	result.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result.DirectoryAnalyses = maps.Clone(base.DirectoryAnalyses)
	if result.DirectoryAnalyses == nil {
		result.DirectoryAnalyses = map[string]DirectoryAnalysis{}
	}
	for previous := range result.DirectoryAnalyses {
		if within(previous, path) || within(path, previous) {
			delete(result.DirectoryAnalyses, previous)
		}
	}
	if complete && physical.Analysis != nil {
		result.DirectoryAnalyses[path] = DirectoryAnalysis{ObservedAt: result.UpdatedAt, Analysis: physical.Analysis}
	}
	if err := validateIncrementalNodes(nodes); err != nil {
		return nil, err
	}
	return &result, nil
}

// During traversal, measured children consume the unassigned historical bytes.
// Only the final observation may remove unvisited entries or shrink the total.
func mergeDirectoryObservation(old, raw *Node) *Node {
	n := copyNode(raw)
	n.Scanning = true
	if old == nil {
		return n
	}
	previous := map[string]*Node{}
	for _, child := range old.Children {
		previous[child.Path] = child
	}
	for i, child := range n.Children {
		if child.Scanning && previous[child.Path] != nil {
			n.Children[i] = mergeDirectoryObservation(previous[child.Path], child)
			adjustDirectoryTotals(n, child, n.Children[i])
		}
		delete(previous, child.Path)
	}
	for _, child := range old.Children {
		if previous[child.Path] != nil {
			n.Children = append(n.Children, child)
			aggregate(n, child)
		}
	}
	n.DeviceAllocated = nil
	n.Allocated = max(old.Allocated, n.Allocated)
	n.Apparent = max(old.Apparent, n.Apparent)
	n.Files = max(old.Files, n.Files)
	if len(old.DeviceAllocated) == 1 && len(raw.DeviceAllocated) == 1 {
		for dev := range old.DeviceAllocated {
			if _, ok := raw.DeviceAllocated[dev]; ok {
				n.DeviceAllocated = map[string]int64{dev: n.Allocated}
			}
		}
	}
	return n
}
func numberInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case uint64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}
func validateIncrementalNodes(nodes map[string]*Node) error {
	for _, n := range nodes {
		allocated, apparent := int64(0), int64(0)
		for _, child := range n.Children {
			allocated += child.Allocated
			apparent += child.Apparent
		}
		if n.Allocated < allocated || n.Apparent < apparent || n.Files < 0 || n.Errors < 0 {
			return fmt.Errorf("增量扫描汇总不一致：%s", n.Path)
		}
		var devices int64
		for _, v := range n.DeviceAllocated {
			if v < 0 {
				return fmt.Errorf("设备用量不一致：%s", n.Path)
			}
			devices += v
		}
		if n.DeviceAllocated != nil && devices != n.Allocated {
			return fmt.Errorf("目录与设备用量不一致：%s", n.Path)
		}
	}
	return nil
}
