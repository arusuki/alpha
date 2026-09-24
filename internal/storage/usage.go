package storage

import (
	"path/filepath"
	"sort"
	"strings"

	"project-alpha/internal/fsutil"
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
	HostAllocated, ContainerAllocated        map[string]int64
	HostOnly                                 map[string]bool
	HostBlockers                             map[string]hostBlocker
	Exclusive, Shared, CrossOwner, Unrelated int64
	Limited                                  bool
}
type stringSet map[string]bool

const defaultDockerDataRoot = "/var/lib/docker"

func dockerRootAliases(root string) []string {
	canonical := fsutil.Canonical(root)
	if canonical == root {
		return []string{root}
	}
	return []string{root, canonical}
}

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
	u := &usageIndex{
		Nodes: map[string]*Node{}, Containers: map[string]*ContainerUsage{}, Owners: map[string]*OwnerUsage{},
		HostAllocated: map[string]int64{}, ContainerAllocated: map[string]int64{}, HostOnly: map[string]bool{},
		HostBlockers: map[string]hostBlocker{},
	}
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
	claimedAncestors := map[string]bool{}
	unknownResources := map[string]bool{}
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
			} else {
				unknownResources[path] = true
			}
		}
		if len(ids) > 0 {
			for ancestor := path; !claimedAncestors[ancestor]; ancestor = filepath.Dir(ancestor) {
				claimedAncestors[ancestor] = true
			}
		}
	}
	for _, r := range s.Resources {
		add(r.Path, r.Containers)
	}
	// Incomplete observations affect descendants. Folded references are checked
	// separately: a Host-to-Host hard link does not make ownership unknown.
	for _, n := range order {
		if n.Scanning || n.SizeUnknown {
			unknownResources[n.Path] = true
		}
	}
	dockerRoots := dockerRootAliases(defaultDockerDataRoot)
	if root, ok := s.Docker["root"].(string); ok && filepath.IsAbs(root) {
		dockerRoots = append(dockerRoots, dockerRootAliases(root)...)
	}
	if root, ok := s.Docker["root_canonical"].(string); ok && filepath.IsAbs(root) {
		dockerRoots = append(dockerRoots, root)
	}
	for _, r := range s.Resources {
		for _, kind := range r.Kinds {
			if kind == "docker-root" && filepath.IsAbs(r.Path) {
				dockerRoots = append(dockerRoots, dockerRootAliases(r.Path)...)
			}
		}
	}
	withinDockerData := func(path string) bool {
		for _, root := range dockerRoots {
			if within(path, root) || within(root, path) {
				return true
			}
		}
		return false
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
	unknownMembership := map[string]bool{}
	var hasUnknownMember func(string) bool
	hasUnknownMember = func(path string) bool {
		if path == "" || path == "@root" {
			return false
		}
		if unknown, ok := unknownMembership[path]; ok {
			return unknown
		}
		unknown := unknownResources[path] || (path != "/" && hasUnknownMember(filepath.Dir(path)))
		unknownMembership[path] = unknown
		return unknown
	}
	claims := map[string]stringSet{}
	unknownClaims := map[string]bool{}
	refs := []*Node{}
	for _, n := range order {
		ids := stringSet{}
		for id := range members(n.Path) {
			ids[id] = true
		}
		claims[n.Path] = ids
		unknownClaims[n.Path] = hasUnknownMember(n.Path)
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
		incomingUnknown := unknownClaims[ref.Path]
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
			if incomingUnknown && !unknownClaims[n.Path] {
				unknownClaims[n.Path] = true
				changed = true
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
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		bytes := n.Allocated
		for _, ch := range n.Children {
			bytes -= ch.Allocated
		}
		residualValid := bytes >= 0
		if bytes < 0 {
			bytes = 0
		}
		ids := claims[n.Path]
		hostBytes, containerBytes := int64(0), int64(0)
		// Aggregate failures propagate from their actual child in the reference
		// graph, preserving the path that explains why a parent is blocked.
		local := *n
		for _, child := range n.Children {
			local.Errors -= child.Errors
			local.PermissionDenied -= child.PermissionDenied
			local.Excluded -= child.Excluded
		}
		blocker := localHostBlocker(&local, len(ids) > 0 || claimedAncestors[n.Path], unknownClaims[n.Path], withinDockerData(n.Path), residualValid)
		if n.Kind == "reference" && u.hostReferenceTarget(n.Path) == "" {
			blocker = hostBlocker{n.Path, "unresolved_reference", "inode 引用目标缺失或形成循环"}
		}
		for _, ch := range n.Children {
			hostBytes += u.HostAllocated[ch.Path]
			containerBytes += u.ContainerAllocated[ch.Path]
		}
		if len(ids) == 0 {
			hostBytes += bytes
		} else {
			containerBytes += bytes
		}
		u.HostAllocated[n.Path] = hostBytes
		u.ContainerAllocated[n.Path] = containerBytes
		if blocker.Code == "" && hostBytes+containerBytes != n.Allocated {
			blocker = hostBlocker{n.Path, "inconsistent_totals", "父子目录容量汇总不一致"}
		}
		u.HostOnly[n.Path] = blocker.Code == ""
		if blocker.Code != "" {
			u.HostBlockers[n.Path] = blocker
		}
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
	u.checkHostReferences(order, func(path string, covering *Node) hostBlocker {
		n := *covering
		n.Path = path
		return localHostBlocker(&n, len(members(path)) > 0 || len(claims[covering.Path]) > 0 || claimedAncestors[path], hasUnknownMember(path) || unknownClaims[covering.Path], withinDockerData(path), true)
	})
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
