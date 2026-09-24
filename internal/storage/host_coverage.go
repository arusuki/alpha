package storage

import (
	"path/filepath"
	"sort"

	"project-alpha/internal/httpapi"
)

// Count the complement of the report's path union. Descending mixed nodes
// exposes safe siblings even when their parent itself cannot be reported.
func (t *recordView) hostCoverage(roots, paths []string, offset, limit int) (object, error) {
	if len(roots) == 0 || len(roots) > 10000 || len(paths) > 10000 {
		return nil, httpapi.NewError(400, "Host 对账需要 1–10000 个根目录和最多 10000 个已列路径")
	}
	u := t.usage
	selected, ancestors := map[string]bool{}, map[string]bool{}
	scope := []string{}
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) < len(roots[j]) })
	for _, p := range roots {
		if validateIncrementalPath(p) != nil || u.Nodes[p] == nil {
			return nil, httpapi.NewError(400, "Host 对账根目录不在记录中")
		}
		covered := false
		for _, root := range scope {
			covered = covered || within(p, root)
		}
		if !covered {
			scope = append(scope, p)
		}
	}
	for _, p := range paths {
		inScope := false
		for _, root := range scope {
			inScope = inScope || within(p, root)
		}
		n := u.Nodes[p]
		if !inScope || n == nil || !u.HostOnly[p] || (n.Kind != "directory" && n.Kind != "file") {
			return nil, httpapi.NewError(400, "Host 对账条目不属于可核实的本组路径")
		}
		selected[p] = true
		for parent := filepath.Dir(p); !ancestors[parent]; parent = filepath.Dir(parent) {
			ancestors[parent] = true
		}
	}
	dockerRoots := dockerRootAliases(defaultDockerDataRoot)
	for _, key := range []string{"root", "root_canonical"} {
		if p, ok := t.snapshot.Docker[key].(string); ok && filepath.IsAbs(p) {
			dockerRoots = append(dockerRoots, dockerRootAliases(p)...)
		}
	}
	for _, r := range t.snapshot.Resources {
		for _, kind := range r.Kinds {
			if kind == "docker-root" {
				dockerRoots = append(dockerRoots, r.Path)
			}
		}
	}
	inDocker := func(p string) bool {
		for _, root := range dockerRoots {
			if root != "" && within(p, root) {
				return true
			}
		}
		return false
	}
	var total, covered, docker int64
	remaining := []object{}
	stack := []*Node{}
	for _, root := range scope {
		total += u.HostAllocated[root]
		stack = append(stack, u.Nodes[root])
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		h := u.HostAllocated[n.Path]
		if inDocker(n.Path) {
			docker += h
			continue
		}
		if selected[n.Path] {
			covered += h
			continue
		}
		if h == 0 {
			continue
		}
		if u.HostOnly[n.Path] && !ancestors[n.Path] && (n.Kind == "directory" || n.Kind == "file") {
			remaining = append(remaining, object{"path": n.Path, "host_allocated": h, "kind": "unlisted", "next_tool": "get_host_directory", "host_only": true})
			continue
		}
		residual := h
		for _, ch := range n.Children {
			residual -= u.HostAllocated[ch.Path]
		}
		if residual > 0 {
			row := object{"path": n.Path, "host_allocated": residual, "kind": "residual", "host_only": u.HostOnly[n.Path]}
			if n.Kind == "directory" && n.Omitted > 0 && !n.Scanning && !n.SizeUnknown {
				row["kind"], row["next_tool"], row["depth"] = "folded", "scan_directory", 1
			}
			if blocker, ok := u.HostBlockers[n.Path]; ok {
				row["host_only_blocker"] = blocker
			}
			remaining = append(remaining, row)
		}
		stack = append(stack, n.Children...)
	}
	sort.Slice(remaining, func(i, j int) bool {
		a, b := remaining[i]["host_allocated"].(int64), remaining[j]["host_allocated"].(int64)
		if a == b {
			return remaining[i]["path"].(string) < remaining[j]["path"].(string)
		}
		return a > b
	})
	return object{"snapshot_id": t.snapshot.JobID, "host_allocated": total, "covered_allocated": covered, "docker_allocated": docker, "remaining_allocated": total - covered - docker, "remaining_count": len(remaining), "remaining": remaining[min(offset, len(remaining)):min(offset+limit, len(remaining))], "has_more": offset+limit < len(remaining)}, nil
}
