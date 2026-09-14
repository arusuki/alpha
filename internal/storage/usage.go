package storage

import (
	"path/filepath"
	"sort"
	"strings"
)

type ContainerUsage struct {
	Container Container `json:"container"`
	Exclusive int64     `json:"exclusive"`
	Shared    int64     `json:"shared"`
	Known     bool      `json:"known"`
	Partial   bool      `json:"partial"`
}
type OwnerUsage struct {
	Owner   string `json:"owner"`
	Bytes   int64  `json:"bytes"`
	Shared  int64  `json:"shared"`
	Known   bool   `json:"known"`
	Partial bool   `json:"partial"`
}
type usageIndex struct {
	Nodes                                    map[string]*Node
	Containers                               map[string]*ContainerUsage
	Owners                                   map[string]*OwnerUsage
	Exclusive, Shared, CrossOwner, Unrelated int64
	Limited                                  bool
}
type stringSet map[string]bool

func ownerName(c Container) string {
	if strings.TrimSpace(c.Owner) == "未标注" {
		return ""
	}
	return strings.TrimSpace(c.Owner)
}
func (u *usageIndex) resolve(path string) *Node {
	n := u.Nodes[path]
	seen := stringSet{}
	for n != nil && n.Kind == "reference" {
		if seen[n.Path] {
			return nil
		}
		seen[n.Path] = true
		n = u.Nodes[n.Reference]
	}
	return n
}

// Keep this partition equivalent to dist/usage.js: aliases propagate claims,
// residual bytes include collapsed descendants, and shared bytes are not split.
func buildUsage(s *Snapshot) *usageIndex {
	u := &usageIndex{Nodes: map[string]*Node{}, Containers: map[string]*ContainerUsage{}, Owners: map[string]*OwnerUsage{}}
	for _, c := range s.Containers {
		u.Containers[c.ID] = &ContainerUsage{Container: c}
		name := ownerName(c)
		if u.Owners[name] == nil {
			u.Owners[name] = &OwnerUsage{Owner: name}
		}
	}
	order := []*Node{}
	stack := []*Node{s.Tree}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		u.Nodes[n.Path] = n
		order = append(order, n)
		stack = append(stack, n.Children...)
	}
	resources := map[string]stringSet{}
	add := func(path string, ids []string) {
		if path == "" {
			return
		}
		if resources[path] == nil {
			resources[path] = stringSet{}
		}
		for _, id := range ids {
			if u.Containers[id] != nil {
				resources[path][id] = true
			}
		}
	}
	for _, r := range s.Resources {
		add(r.Path, r.Containers)
	}
	membership := map[string]stringSet{}
	var members func(string) stringSet
	members = func(path string) stringSet {
		if path == "" || path == "@root" {
			return stringSet{}
		}
		if cached := membership[path]; cached != nil {
			return cached
		}
		ids := stringSet{}
		if path != "/" {
			for id := range members(filepath.Dir(path)) {
				ids[id] = true
			}
		}
		for id := range resources[path] {
			ids[id] = true
		}
		membership[path] = ids
		return ids
	}
	claims := map[string]stringSet{}
	refs := []*Node{}
	for _, n := range order {
		ids := stringSet{}
		for id := range members(n.Path) {
			ids[id] = true
		}
		claims[n.Path] = ids
		if n.Kind == "reference" {
			refs = append(refs, n)
		}
	}
	for len(refs) > 0 {
		ref := refs[len(refs)-1]
		refs = refs[:len(refs)-1]
		target := u.resolve(ref.Path)
		if target == nil {
			continue
		}
		incoming := claims[ref.Path]
		pending := []*Node{target}
		for len(pending) > 0 {
			n := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			changed := false
			for id := range incoming {
				if !claims[n.Path][id] {
					claims[n.Path][id] = true
					changed = true
				}
			}
			if !changed {
				continue
			}
			if n.Kind == "reference" {
				refs = append(refs, n)
			}
			pending = append(pending, n.Children...)
		}
	}
	incomplete := stringSet{}
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		bad := n.Scanning || n.SizeUnknown || n.Errors > 0 || n.Excluded > 0 || n.Kind == "excluded" || n.Kind == "unreadable" || (n.Kind == "reference" && u.resolve(n.Path) == nil)
		for _, ch := range n.Children {
			bad = bad || incomplete[ch.Path]
		}
		incomplete[n.Path] = bad
	}
	for path, ids := range resources {
		n := u.resolve(path)
		known := n != nil && n.Kind != "excluded" && n.Kind != "unreadable"
		for id := range ids {
			r := u.Containers[id]
			r.Known = r.Known || known
			r.Partial = r.Partial || n == nil || (n != nil && incomplete[n.Path])
		}
	}
	u.Limited = numberInt64(s.Scan["omitted_references"]) > 0
	if limited, _ := s.Scan["lazy_accounting_limited"].(bool); limited {
		u.Limited = true
	}
	for _, r := range u.Containers {
		c := r.Container
		r.Partial = r.Partial || u.Limited || c.UpperPath == nil
		if c.WritableLayer != nil && c.WritableLayer.Status != "complete" {
			r.Partial = true
		}
		for _, m := range c.Mounts {
			if (m.Type == "bind" || m.Type == "volume") && m.Source == nil {
				r.Partial = true
			}
		}
	}
	for _, n := range order {
		bytes := n.Allocated
		for _, ch := range n.Children {
			bytes -= ch.Allocated
		}
		if bytes < 0 {
			bytes = 0
		}
		ids := claims[n.Path]
		if len(ids) == 0 {
			u.Unrelated += bytes
			continue
		}
		shared := len(ids) > 1
		if shared {
			u.Shared += bytes
		} else {
			u.Exclusive += bytes
		}
		names := stringSet{}
		for id := range ids {
			r := u.Containers[id]
			if shared {
				r.Shared += bytes
			} else {
				r.Exclusive += bytes
			}
			names[ownerName(r.Container)] = true
		}
		if len(names) == 1 && (!names[""] || len(ids) == 1) {
			for name := range names {
				u.Owners[name].Bytes += bytes
			}
		} else {
			u.CrossOwner += bytes
			for name := range names {
				u.Owners[name].Shared += bytes
			}
		}
	}
	for _, r := range u.Containers {
		o := u.Owners[ownerName(r.Container)]
		o.Known = o.Known || r.Known
		o.Partial = o.Partial || r.Partial
	}
	return u
}
func (u *usageIndex) rankedContainers() []*ContainerUsage {
	rows := []*ContainerUsage{}
	for _, r := range u.Containers {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Exclusive == rows[j].Exclusive {
			return rows[i].Container.Name < rows[j].Container.Name
		}
		return rows[i].Exclusive > rows[j].Exclusive
	})
	return rows
}
func (u *usageIndex) rankedOwners() []*OwnerUsage {
	rows := []*OwnerUsage{}
	for _, r := range u.Owners {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Bytes == rows[j].Bytes {
			return rows[i].Owner < rows[j].Owner
		}
		return rows[i].Bytes > rows[j].Bytes
	})
	return rows
}
