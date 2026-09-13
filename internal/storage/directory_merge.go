package storage

import (
	"maps"
)

func copyNode(n *Node) *Node {
	copy := *n
	copy.DeviceAllocated = maps.Clone(n.DeviceAllocated)
	copy.Children = append([]*Node{}, n.Children...)
	return &copy
}
func validDeviceTotal(n *Node) bool {
	if n.DeviceAllocated == nil {
		return n.Allocated == 0
	}
	var total int64
	for _, v := range n.DeviceAllocated {
		if v < 0 {
			return false
		}
		total += v
	}
	return total == n.Allocated
}

// Copy the selected branch's ancestors and propagate its measured difference.
func applyDirectoryReplacement(n *Node, path string, replacement *Node) *Node {
	if n.Path == path {
		return replacement
	}
	for i, child := range n.Children {
		if !within(path, child.Path) {
			continue
		}
		copy := copyNode(n)
		after := applyDirectoryReplacement(child, path, replacement)
		copy.Children[i] = after
		adjustDirectoryTotals(copy, child, after)
		return copy
	}
	return n
}

// Preserve uncertain outside references until their inode identity can be verified.
func preserveSharedClaims(base *Snapshot, physical *physicalScan, path string) *Node {
	old := snapshotNodes(base.Tree)
	claims := map[string]bool{}
	for _, n := range old {
		if !within(n.Path, path) && n.Reference != "" && within(n.Reference, path) {
			claims[n.Reference] = true
		}
	}
	prior, observed := map[string]InodeRecord{}, map[string]InodeRecord{}
	for _, r := range base.Accounting {
		prior[r.Path] = r
	}
	for _, r := range physical.Accounting {
		observed[r.Path] = r
	}
	var visit func(*Node) *Node
	visit = func(source *Node) *Node {
		n := copyNode(source)
		previous := old[n.Path]
		before, after := prior[n.Path], observed[n.Path]
		uncertain := claims[n.Path] && (before.Path == "" || before.Device != after.Device || before.Inode != after.Inode)
		if previous != nil && (uncertain || n.Errors > 0 && n.Reason != "") {
			kept := copyNode(previous)
			kept.SizeUnknown = true
			kept.Errors, kept.PermissionDenied = n.Errors, n.PermissionDenied
			kept.Reason = n.Reason
			if uncertain {
				kept.Reason = "外部路径仍引用历史文件，当前目录无法确认其变更；保留历史占用待核对"
			}
			return kept
		}
		for i, child := range n.Children {
			replacement := visit(child)
			adjustDirectoryTotals(n, child, replacement)
			n.Children[i] = replacement
		}
		if previous != nil {
			present := map[string]bool{}
			for _, child := range n.Children {
				present[child.Path] = true
			}
			for _, child := range previous.Children {
				if claims[child.Path] && !present[child.Path] {
					kept := copyNode(child)
					kept.SizeUnknown = true
					kept.Reason = "此路径已消失，外部引用尚未核对；保留历史占用"
					n.Children = append(n.Children, kept)
					devicesKnown := validDeviceTotal(n) && validDeviceTotal(kept)
					aggregate(n, kept)
					if !devicesKnown {
						n.DeviceAllocated = nil
					}
				}
			}
		}
		return n
	}
	return visit(physical.Tree.Children[0])
}

// Apply a single child delta in constant time. Scanning all siblings for every
// child would make a directory with thousands of files quadratic to merge.
func adjustDirectoryTotals(parent, before, after *Node) {
	parent.Allocated += after.Allocated - before.Allocated
	parent.Apparent += after.Apparent - before.Apparent
	parent.Files += after.Files - before.Files
	parent.Errors += after.Errors - before.Errors
	parent.Excluded += after.Excluded - before.Excluded
	parent.PermissionDenied += after.PermissionDenied - before.PermissionDenied
	parent.OmittedReferences += after.OmittedReferences - before.OmittedReferences
	if parent.DeviceAllocated != nil && validDeviceTotal(before) && validDeviceTotal(after) {
		for dev, bytes := range before.DeviceAllocated {
			parent.DeviceAllocated[dev] -= bytes
		}
		for dev, bytes := range after.DeviceAllocated {
			parent.DeviceAllocated[dev] += bytes
		}
	} else {
		parent.DeviceAllocated = nil
	}
}
