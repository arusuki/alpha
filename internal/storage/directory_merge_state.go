package storage

import "path/filepath"

// The baseline is immutable throughout one exploration. Cache the context that
// cannot change and rebuild only the observed branch for each checkpoint.
type directoryMerge struct {
	base              *Snapshot
	path              string
	baseNodes         map[string]*Node
	outsideCount      int
	outsideLimited    bool
	outsideReferences []*Node
	outsideDeferred   map[string]bool
	outsideAccounting []InodeRecord
	insideAccounting  []InodeRecord
	claims            map[string]bool
	priorClaims       map[string]InodeRecord
}

func markDeferred(deferred map[string]bool, path string) {
	for {
		deferred[path] = true
		parent := filepath.Dir(path)
		if parent == path {
			return
		}
		path = parent
	}
}

func newDirectoryMerge(base *Snapshot, path string) (*directoryMerge, error) {
	m := &directoryMerge{base: base, path: path, baseNodes: snapshotNodes(base.Tree), outsideDeferred: map[string]bool{}, claims: map[string]bool{}, priorClaims: map[string]InodeRecord{}}
	if err := validateIncrementalNodes(m.baseNodes); err != nil {
		return nil, err
	}
	for _, n := range m.baseNodes {
		if within(n.Path, path) {
			continue
		}
		m.outsideCount++
		if n.Scanning || n.SizeUnknown {
			m.outsideLimited = true
			markDeferred(m.outsideDeferred, n.Path)
		}
		if n.Kind == "reference" {
			m.outsideReferences = append(m.outsideReferences, n)
		}
		if n.Reference != "" && within(n.Reference, path) {
			m.claims[n.Reference] = true
		}
	}
	for _, r := range base.Accounting {
		if within(r.Path, path) {
			m.insideAccounting = append(m.insideAccounting, r)
		} else {
			m.outsideAccounting = append(m.outsideAccounting, r)
		}
		if m.claims[r.Path] {
			m.priorClaims[r.Path] = r
		}
	}
	return m, nil
}

// Find the physical branch without allocating an index of unrelated nodes.
func findSnapshotNode(tree *Node, path string) *Node {
	for tree != nil && tree.Path != path {
		var next *Node
		for _, child := range tree.Children {
			if within(path, child.Path) {
				next = child
				break
			}
		}
		tree = next
	}
	return tree
}

func directoryAncestors(tree *Node, path string) map[string]*Node {
	nodes := map[string]*Node{}
	for tree != nil && tree.Path != path {
		nodes[tree.Path] = tree
		var next *Node
		for _, child := range tree.Children {
			if within(path, child.Path) {
				next = child
				break
			}
		}
		tree = next
	}
	return nodes
}
