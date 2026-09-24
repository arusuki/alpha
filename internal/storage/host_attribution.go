package storage

import "path/filepath"

type hostBlocker struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Unlike resolve, this also returns a physical path whose node was folded.
// Empty denotes a broken reference or an actual cycle of alias nodes.
func (u *usageIndex) hostReferenceTarget(path string) string {
	seen := map[string]bool{}
	for {
		n := u.Nodes[path]
		if n == nil || n.Kind != "reference" {
			return path
		}
		if seen[path] || n.Reference == "" {
			return ""
		}
		seen[path] = true
		path = n.Reference
	}
}

func localHostBlocker(n *Node, claimed, unknown, docker, validTotals bool) hostBlocker {
	code, message := "", ""
	switch {
	case claimed:
		code, message = "container_reference", "该路径或其子路径存在容器引用（包括零字节引用）"
	case docker:
		code, message = "docker_data", "该路径位于 Docker 数据目录内或包含 Docker 数据目录"
	case n.Scanning:
		code, message = "scanning", "目录仍在扫描"
	case n.SizeUnknown:
		code, message = "size_unknown", "目录占用尚未核实"
	case n.PermissionDenied > 0:
		code, message = "permission_denied", "目录存在权限不足，无法完整读取"
	case n.Errors > 0 || n.Kind == "unreadable":
		code, message = "scan_error", "目录存在读取错误"
	case n.Excluded > 0 || n.Kind == "excluded":
		code, message = "excluded", "目录包含未扫描的排除项"
	case unknown:
		code, message = "unknown_ownership", "关联资源或祖先目录的归属尚未核实"
	case !validTotals:
		code, message = "inconsistent_totals", "父子目录容量汇总不一致"
	case n.Kind != "root" && n.Kind != "directory" && n.Kind != "file" && n.Kind != "symlink" && n.Kind != "reference":
		code, message = "unsupported_kind", "节点不是可核实的文件或目录"
	}
	return hostBlocker{n.Path, code, message}
}

// Ordinary omitted entries were still measured. Folded inode aliases retain a
// source directory and a target frontier, enough to certify Host-only groups
// without retaining every hard link. Their exact byte attribution may remain
// limited; never assign the entire frontier's bytes to a container.
func (u *usageIndex) checkHostReferences(order []*Node, hiddenBlocker func(string, *Node) hostBlocker) {
	dependents := map[string][]string{}
	children := map[string][]string{}
	paths := make([]string, 0, len(order))
	for _, n := range order {
		paths = append(paths, n.Path)
		for _, child := range n.Children {
			dependents[child.Path] = append(dependents[child.Path], n.Path)
			children[n.Path] = append(children[n.Path], child.Path)
		}
		if n.Kind == "reference" {
			if target := u.resolve(n.Path); target != nil {
				dependents[target.Path] = append(dependents[target.Path], n.Path)
			}
		}
	}
	// Add hidden frontiers as graph vertices, without inventing snapshot nodes
	// or bytes. Only a measured, collapsed directory can cover a missing target.
	hidden := []string{}
	targets := []string{}
	for _, n := range order {
		frontiers := n.OmittedReferenceTargets
		if n.Kind == "reference" && u.resolve(n.Path) == nil {
			if target := u.hostReferenceTarget(n.Path); target != "" {
				frontiers = append(append([]string{}, frontiers...), target)
			}
		}
		for _, target := range frontiers {
			targets = append(targets, target)
			if _, exists := u.HostOnly[target]; !exists {
				blocker := hostBlocker{target, "unresolved_reference", "折叠 inode 引用目标没有已扫描的目录证据"}
				for p := filepath.Dir(target); ; p = filepath.Dir(p) {
					if covering := u.Nodes[p]; covering != nil {
						if covering.Kind == "directory" && covering.Omitted > 0 {
							blocker = hiddenBlocker(target, covering)
						}
						break
					}
					if p == filepath.Dir(p) {
						break
					}
				}
				u.HostOnly[target] = blocker.Code == ""
				if blocker.Code != "" {
					u.HostBlockers[target] = blocker
				}
				hidden = append(hidden, target)
				paths = append(paths, target)
			}
			// Both sides must be Host-only, including chains and cycles of
			// folded references and claims with zero allocated bytes.
			dependents[target] = append(dependents[target], n.Path)
			dependents[n.Path] = append(dependents[n.Path], target)
		}
	}
	for _, path := range hidden {
		for p := filepath.Dir(path); ; p = filepath.Dir(p) {
			if _, exists := u.HostOnly[p]; exists {
				dependents[path] = append(dependents[path], p)
				children[p] = append(children[p], path)
				break
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	// A frontier denotes an unknown inode somewhere in that subtree. If its
	// source is unsafe, protect all descendants and resolved aliases there.
	// Expand each vertex only once, even when many aliases share a frontier.
	expanded := map[string]bool{}
	for len(targets) > 0 {
		path := targets[len(targets)-1]
		targets = targets[:len(targets)-1]
		if expanded[path] {
			continue
		}
		expanded[path] = true
		for _, child := range children[path] {
			dependents[path] = append(dependents[path], child)
			targets = append(targets, child)
		}
		if n := u.Nodes[path]; n != nil && n.Kind == "reference" {
			if target := u.resolve(path); target != nil {
				dependents[path] = append(dependents[path], target.Path)
				targets = append(targets, target.Path)
			}
		}
	}
	unsafe := []string{}
	for _, path := range paths {
		if !u.HostOnly[path] {
			unsafe = append(unsafe, path)
		}
	}
	for len(unsafe) > 0 {
		path := unsafe[len(unsafe)-1]
		unsafe = unsafe[:len(unsafe)-1]
		for _, dependent := range dependents[path] {
			if u.HostOnly[dependent] {
				u.HostOnly[dependent] = false
				u.HostBlockers[dependent] = u.HostBlockers[path]
				unsafe = append(unsafe, dependent)
			}
		}
	}
}
