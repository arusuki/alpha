package storage

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"project-alpha/internal/httpapi"
)

type recordView struct {
	snapshot *Snapshot
	usage    *usageIndex
}

// Query reads the current committed record, including all published exploration.
// It never scans and does not retain a session-specific snapshot cache.
func (s *Service) Query(id, operation string, fields map[string]json.RawMessage) (object, error) {
	allowed := map[string][]string{
		"overview": {}, "containers": {"query", "sort_by", "offset", "limit"},
		"owners": {"offset", "limit"}, "container": {"container"}, "directory": {"path", "offset", "limit"},
		"nodes": {"paths"}, "host_roots": {"offset", "limit"},
		"host_directory": {"path", "offset", "limit"}, "host_nodes": {"paths"},
		"host_coverage": {"roots", "paths", "offset", "limit"},
	}
	keys, ok := allowed[operation]
	if !ok {
		return nil, httpapi.NewError(400, "未知记录查询")
	}
	for key, value := range fields {
		found := false
		for _, k := range keys {
			if key == k {
				found = true
			}
		}
		if !found || string(value) == "null" {
			return nil, httpapi.NewError(400, "记录查询参数无效")
		}
	}
	var args struct {
		Path, Container, Query string
		SortBy                 string `json:"sort_by"`
		Offset, Limit          int
		Paths                  []string
		Roots                  []string
	}
	args.Limit = 30
	args.SortBy = "exclusive"
	if err := json.Unmarshal([]byte(httpapi.JSONText(fields)), &args); err != nil {
		return nil, httpapi.NewError(400, "记录查询参数类型无效")
	}
	if args.Limit < 1 || args.Limit > 50 || args.Offset < 0 || args.Offset > 1000000 {
		return nil, httpapi.NewError(400, "分页 limit 需为 1–50，offset 需为 0–1000000")
	}
	snapshot, err := s.ReadSnapshot(id)
	if err != nil {
		return nil, err
	}
	t := &recordView{snapshot: snapshot, usage: buildUsage(snapshot)}
	result, err := func() (object, error) {
		switch operation {
		case "host_coverage":
			return t.hostCoverage(args.Roots, args.Paths, args.Offset, args.Limit)
		case "overview":
			return t.overview(), nil
		case "containers":
			if args.SortBy != "exclusive" && args.SortBy != "writable" && args.SortBy != "docker_logical" {
				return nil, httpapi.NewError(400, "无效排序字段")
			}
			rows := []*ContainerUsage{}
			q := strings.ToLower(args.Query)
			for _, r := range t.usage.rankedContainers() {
				c := r.Container
				if q == "" || strings.Contains(strings.ToLower(c.Name+" "+c.Owner+" "+c.State), q) {
					rows = append(rows, r)
				}
			}
			value := func(r *ContainerUsage) int64 {
				if args.SortBy == "writable" {
					if r.Container.WritableLayer != nil && r.Container.WritableLayer.Allocated != nil {
						return *r.Container.WritableLayer.Allocated
					}
					return -1
				}
				if args.SortBy == "docker_logical" {
					if r.Container.SizeRW != nil {
						return *r.Container.SizeRW
					}
					return -1
				}
				return r.Exclusive
			}
			sort.SliceStable(rows, func(i, j int) bool { return value(rows[i]) > value(rows[j]) })
			items := []object{}
			for i := min(args.Offset, len(rows)); i < min(args.Offset+args.Limit, len(rows)); i++ {
				items = append(items, containerSummary(rows[i]))
			}
			return object{"snapshot_id": t.snapshot.JobID, "total": len(rows), "items": items, "has_more": args.Offset+args.Limit < len(rows)}, nil
		case "owners":
			rows := t.usage.rankedOwners()
			return object{"snapshot_id": t.snapshot.JobID, "total": len(rows), "items": rows[min(args.Offset, len(rows)):min(args.Offset+args.Limit, len(rows))], "cross_owner_shared": t.usage.CrossOwner}, nil
		case "host_roots":
			paths := []string{}
			seen := map[string]bool{}
			for _, resource := range t.snapshot.Resources {
				for _, kind := range resource.Kinds {
					if kind == "host" && filepath.IsAbs(resource.Path) && filepath.Clean(resource.Path) == resource.Path && !seen[resource.Path] {
						seen[resource.Path] = true
						paths = append(paths, resource.Path)
					}
				}
			}
			// Nested explicit roots cover the same physical subtree. Keep the
			// outer root once, even when it was recorded later in Resources.
			sort.Slice(paths, func(i, j int) bool {
				if len(paths[i]) == len(paths[j]) {
					return paths[i] < paths[j]
				}
				return len(paths[i]) < len(paths[j])
			})
			rows := []object{}
			for _, p := range paths {
				covered := false
				for _, row := range rows {
					if within(p, row["path"].(string)) {
						covered = true
						break
					}
				}
				if covered {
					continue
				}
				name, allocated := filepath.Base(p), int64(0)
				if node := t.usage.Nodes[p]; node != nil {
					name, allocated = node.Name, t.usage.HostAllocated[p]
				}
				rows = append(rows, object{"path": p, "name": name, "host_allocated": allocated})
			}
			sort.Slice(rows, func(i, j int) bool {
				left, right := rows[i]["host_allocated"].(int64), rows[j]["host_allocated"].(int64)
				if left == right {
					return rows[i]["path"].(string) < rows[j]["path"].(string)
				}
				return left > right
			})
			return object{"snapshot_id": t.snapshot.JobID, "total": len(rows), "items": rows[min(args.Offset, len(rows)):min(args.Offset+args.Limit, len(rows))], "has_more": args.Offset+args.Limit < len(rows)}, nil
		case "container":
			var row *ContainerUsage
			for _, r := range t.usage.Containers {
				if r.Container.ID == args.Container || r.Container.Name == args.Container {
					row = r
					break
				}
			}
			if row == nil {
				return nil, httpapi.NewError(404, "容器不存在于本次快照")
			}
			c := row.Container
			sources := []object{}
			entrances := t.snapshot.filesystemEntrances()
			for _, r := range t.snapshot.Resources {
				for _, id := range r.Containers {
					if id == c.ID {
						sources = append(sources, object{"path": r.Path, "kinds": r.Kinds, "access_only": r.accessOnly(entrances), "referencing_containers": r.Containers, "node": nodeSummary(t.usage.resolve(r.Path))})
						break
					}
				}
			}
			return object{"snapshot_id": t.snapshot.JobID, "observed_at": t.snapshot.FinishedAt, "usage": row, "sources": sources, "note": "挂载源可能共享或互相包含，不能直接把 sources 相加；access_only 为宿主机分区入口访问引用，其容量不属于容器用量"}, nil

		case "directory", "nodes", "host_directory", "host_nodes":
			job, err := s.Job(id)
			if err != nil {
				return nil, err
			}
			var config Config
			if err = json.Unmarshal([]byte(httpapi.JSONText(job["config"])), &config); err != nil {
				return nil, err
			}
			if operation == "nodes" || operation == "host_nodes" {
				// Resolve a batch against one immutable revision. Report validation
				// must not reload and rebuild the whole record for every finding.
				items := []object{}
				for _, requested := range args.Paths {
					item := object{"requested_path": requested}
					checked, err := validateDetailPath(requested, config, s.DB.Directory)
					if err != nil {
						item["error"] = err.Error()
					} else if operation == "host_nodes" {
						node := t.usage.resolve(checked)
						summary := hostNodeSummary(t.usage, node)
						if node != nil && node.Path != checked {
							summary["host_only"] = false
						}
						item["node"] = summary
					} else {
						item["node"] = nodeSummary(t.usage.resolve(checked))
					}
					items = append(items, item)
				}
				return object{"snapshot_id": id, "items": items}, nil
			}
			path, err := validateDetailPath(args.Path, config, s.DB.Directory)
			if err != nil {
				return nil, httpapi.NewError(400, err.Error())
			}
			var result object
			if operation == "host_directory" {
				result = hostDirectoryResult(snapshot, t.usage, path, args.Offset, args.Limit)
			} else {
				result = directoryResult(snapshot, t.usage, path, args.Offset, args.Limit)
			}
			if observation, ok := snapshot.DirectoryAnalyses[path]; ok {
				result["analysis"] = observation.Analysis
				result["analysis_observed_at"] = observation.ObservedAt
			}
			return result, nil
		}
		return nil, httpapi.NewError(400, "未知记录查询")
	}()
	if err != nil {
		return nil, err
	}
	result["revision"] = snapshot.Revision
	result["updated_at"] = snapshot.UpdatedAt
	return result, nil
}
func (t *recordView) overview() object {
	roots := []object{}
	for _, n := range t.snapshot.Tree.Children {
		roots = append(roots, nodeSummary(n))
	}
	containers := t.usage.rankedContainers()
	top := []object{}
	for i, r := range containers {
		if i >= 10 {
			break
		}
		top = append(top, containerSummary(r))
	}
	owners := t.usage.rankedOwners()
	if len(owners) > 20 {
		owners = owners[:20]
	}
	warnings := t.snapshot.Warnings
	if len(warnings) > 20 {
		warnings = warnings[:20]
	}
	return object{"snapshot_id": t.snapshot.JobID, "observed_at": t.snapshot.FinishedAt, "host": t.snapshot.Host, "roots": roots, "docker": t.snapshot.Docker, "filesystems": t.snapshot.Filesystems, "allocated": t.snapshot.Tree.Allocated, "files": t.snapshot.Tree.Files, "containers": len(containers), "exclusive": t.usage.Exclusive, "shared": t.usage.Shared, "cross_owner_shared": t.usage.CrossOwner, "unrelated": t.usage.Unrelated, "attribution_limited": t.usage.Limited, "scan": t.snapshot.Scan, "warnings": warnings, "warning_count": len(t.snapshot.Warnings), "top_containers": top, "top_owners": owners, "note": "统计为扫描期间观察值，非实时；全盘合计含可写层，不能重复相加。未解释空间不等于可回收空间。"}
}
func containerSummary(r *ContainerUsage) object {
	return object{"id": r.Container.ID, "name": r.Container.Name, "owner": ownerName(r.Container), "state": r.Container.State, "exclusive": r.Exclusive, "shared": r.Shared, "known": r.Known, "partial": r.Partial, "writable_layer": r.Container.WritableLayer, "docker_logical": r.Container.SizeRW}
}

func nodeSummary(n *Node) object {
	if n == nil {
		return object{"known": false, "reason": "快照未保留该路径；请从记录中已有的父目录逐层增量探索，不能据此判为不存在或零占用"}
	}
	return object{"name": n.Name, "path": n.Path, "kind": n.Kind, "allocated": n.Allocated, "apparent": n.Apparent, "files": n.Files, "errors": n.Errors, "excluded_entries": n.Excluded, "permission_denied": n.PermissionDenied, "omitted_entries": n.Omitted, "omitted_references": n.OmittedReferences, "reference": n.Reference, "reason": n.Reason, "scanning": n.Scanning, "size_unknown": n.SizeUnknown, "known": !n.SizeUnknown && !n.Scanning && n.Kind != "unreadable" && n.Kind != "excluded"}
}
func hostNodeSummary(u *usageIndex, n *Node) object {
	result := nodeSummary(n)
	result["host_allocated"] = int64(0)
	result["container_allocated"] = int64(0)
	result["host_only"] = false
	if n != nil {
		result["host_allocated"] = u.HostAllocated[n.Path]
		result["container_allocated"] = u.ContainerAllocated[n.Path]
		result["host_only"] = u.HostOnly[n.Path] && (n.Kind == "directory" || n.Kind == "file")
		if blocker, ok := u.HostBlockers[n.Path]; ok {
			result["host_only_blocker"] = blocker
		}
	}
	return result
}
func directoryResult(s *Snapshot, u *usageIndex, path string, offset, limit int) object {
	return directoryResultWithHost(s, u, path, offset, limit, false)
}
func hostDirectoryResult(s *Snapshot, u *usageIndex, path string, offset, limit int) object {
	result := directoryResultWithHost(s, u, path, offset, limit, true)
	if node, ok := result["node"].(object); ok && node["path"] != path {
		node["host_only"] = false
	}
	return result
}
func directoryResultWithHost(s *Snapshot, u *usageIndex, path string, offset, limit int, host bool) object {
	n := u.resolve(path)
	summarize := func(n *Node) object { return nodeSummary(n) }
	if host {
		summarize = func(n *Node) object { return hostNodeSummary(u, n) }
	}
	result := object{"snapshot_id": s.JobID, "observed_at": s.FinishedAt, "requested_path": path, "node": summarize(n), "offset": offset, "limit": limit}
	if n == nil {
		return result
	}
	children := append([]*Node{}, n.Children...)
	sort.Slice(children, func(i, j int) bool {
		left, right := children[i].Allocated, children[j].Allocated
		if host {
			left, right = u.HostAllocated[children[i].Path], u.HostAllocated[children[j].Path]
		}
		if left == right {
			return children[i].Path < children[j].Path
		}
		return left > right
	})
	result["total_entries"] = len(children)
	residual := n.Allocated
	for _, child := range children {
		residual -= child.Allocated
	}
	result["self_and_omitted_allocated"] = max(int64(0), residual)
	if host {
		hostResidual := u.HostAllocated[n.Path]
		for _, child := range children {
			hostResidual -= u.HostAllocated[child.Path]
		}
		result["self_and_omitted_host_allocated"] = max(int64(0), hostResidual)
	}
	entries := []object{}
	for i := min(offset, len(children)); i < min(offset+limit, len(children)); i++ {
		child := children[i]
		entry := summarize(child)
		if child.Kind == "reference" {
			entry["reference_target"] = summarize(u.resolve(child.Path))
		}
		entries = append(entries, entry)
	}
	result["entries"] = entries
	result["has_more"] = offset+limit < len(children)
	return result
}
