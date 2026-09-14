// Package process reconstructs the forest of running processes inside each
// container by tracking Tetragon process lifecycle events.
package process

import (
	"sort"
	"strings"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
)

// Process is one active process in an exported forest. Children are ordered by
// start time, then PID.
type Process struct {
	ExecID    string     `json:"exec_id"`
	PID       uint32     `json:"pid"`
	Binary    string     `json:"binary"`
	Arguments string     `json:"arguments,omitempty"`
	Command   string     `json:"command"`
	CWD       string     `json:"cwd,omitempty"`
	UID       uint32     `json:"uid"`
	StartedAt string     `json:"started_at,omitempty"`
	Children  []*Process `json:"children"`
}

// Container groups the process forest observed inside one container.
type Container struct {
	ID           string     `json:"id"`
	ProcessCount int        `json:"process_count"`
	Roots        []*Process `json:"roots"`
}

// Forest is the point-in-time result of tracking process lifecycle events.
type Forest struct {
	CapturedAt      time.Time    `json:"captured_at"`
	DurationSeconds float64      `json:"duration_seconds"`
	Bootstrapped    bool         `json:"bootstrapped"`
	Containers      []*Container `json:"containers"`
	Host            *Container   `json:"host,omitempty"`
}

// Options carries the metadata recorded alongside a snapshot.
type Options struct {
	IncludeHost  bool
	CapturedAt   time.Time
	Duration     time.Duration
	Bootstrapped bool
}

// node is the mutable view of a process while it is active. It keeps the
// identity needed to splice the tree when a process exits.
type node struct {
	execID     string
	parentExec string
	container  string
	pid        uint32
	uid        uint32
	binary     string
	arguments  string
	cwd        string
	start      time.Time
	parent     *node
	children   []*node
}

// Builder maintains the active process forest. Events may arrive in any order:
// a process whose parent has not been observed yet is held back and attached
// once the parent appears.
type Builder struct {
	nodes   map[string]*node
	pending map[string][]*node
}

func NewBuilder() *Builder {
	return &Builder{nodes: map[string]*node{}, pending: map[string][]*node{}}
}

// Len reports how many processes the builder currently considers active.
func (b *Builder) Len() int { return len(b.nodes) }

// Apply dispatches one lifecycle event. Process execution adds or refreshes a
// node; process exit removes it and splices its children to the closest
// surviving ancestor.
func (b *Builder) Apply(resp *tetragon.GetEventsResponse) {
	if resp == nil {
		return
	}
	if exec := resp.GetProcessExec(); exec != nil {
		b.Observe(exec.GetProcess())
		return
	}
	if exit := resp.GetProcessExit(); exit != nil {
		b.ObserveExit(exit.GetProcess())
	}
}

// Observe records a process as running, creating its node on first sight.
func (b *Builder) Observe(p *tetragon.Process) {
	if p == nil || p.GetExecId() == "" {
		return
	}
	n, seen := b.nodes[p.GetExecId()]
	if !seen {
		n = &node{execID: p.GetExecId()}
		b.nodes[n.execID] = n
	}
	n.parentExec = p.GetParentExecId()
	n.container = containerID(p)
	n.pid = p.GetPid().GetValue()
	n.uid = p.GetUid().GetValue()
	n.binary = p.GetBinary()
	n.arguments = p.GetArguments()
	n.cwd = p.GetCwd()
	if ts := p.GetStartTime(); ts != nil {
		n.start = ts.AsTime()
	}
	if !seen {
		// Both directions are keyed by exec IDs that only exist now.
		b.adopt(n)
		b.attach(n)
	}
}

// ObserveAll records every process of a bootstrap listing.
func (b *Builder) ObserveAll(processes []*tetragon.Process) {
	for _, p := range processes {
		b.Observe(p)
	}
}

// ObserveExit drops a process from the active forest. Its children move up to
// the exited process' parent so the remaining tree stays connected.
func (b *Builder) ObserveExit(p *tetragon.Process) {
	if p == nil {
		return
	}
	n, active := b.nodes[p.GetExecId()]
	if !active {
		return
	}
	delete(b.nodes, n.execID)
	// A process that exits before its parent was observed must also leave the
	// waiting list, otherwise the parent would resurrect it on arrival.
	if waiting := b.pending[n.parentExec]; len(waiting) > 0 {
		if b.pending[n.parentExec] = removeNode(waiting, n); len(b.pending[n.parentExec]) == 0 {
			delete(b.pending, n.parentExec)
		}
	}
	if n.parent != nil {
		n.parent.children = removeNode(n.parent.children, n)
	}
	target := n.parent
	for _, child := range n.children {
		child.parent = target
		if target != nil {
			target.children = append(target.children, child)
		}
	}
	n.children = nil
	n.parent = nil
}

// adopt attaches children that arrived before their parent was observed.
func (b *Builder) adopt(parent *node) {
	waiting := b.pending[parent.execID]
	if len(waiting) == 0 {
		return
	}
	delete(b.pending, parent.execID)
	for _, child := range waiting {
		if child.parent != nil {
			continue
		}
		child.parent = parent
		parent.children = append(parent.children, child)
	}
}

// attach links a node to its parent, or parks it until the parent is observed.
func (b *Builder) attach(n *node) {
	if n.parentExec == "" || n.parentExec == n.execID {
		return
	}
	if parent, ok := b.nodes[n.parentExec]; ok {
		n.parent = parent
		parent.children = append(parent.children, n)
		return
	}
	b.pending[n.parentExec] = append(b.pending[n.parentExec], n)
}

// Snapshot renders the current forest. A process belongs to its own container,
// or to the nearest ancestor that has one, so a container's tree stays together
// even though its init process is a child of the host-side runtime. Processes
// without any container are only included when opts.IncludeHost is set.
func (b *Builder) Snapshot(opts Options) *Forest {
	forest := &Forest{
		CapturedAt:      opts.CapturedAt,
		DurationSeconds: opts.Duration.Seconds(),
		Bootstrapped:    opts.Bootstrapped,
		Containers:      []*Container{},
	}
	effective := make(map[*node]string, len(b.nodes))
	for _, n := range b.nodes {
		if n.parent == nil {
			resolveContainer(n, "", effective)
		}
	}
	roots := map[string][]*node{}
	counts := map[string]int{}
	order := []string{}
	for _, n := range b.nodes {
		id := effective[n]
		// counts doubles as the first-seen marker: roots[id] is only created
		// for root nodes, so it cannot be used to detect a new container.
		if counts[id] == 0 {
			order = append(order, id)
		}
		counts[id]++
		if parent := n.parent; parent == nil || effective[parent] != id {
			roots[id] = append(roots[id], n)
		}
	}
	for _, id := range order {
		sortNodes(roots[id])
		container := &Container{ID: id, ProcessCount: counts[id], Roots: []*Process{}}
		for _, root := range roots[id] {
			container.Roots = append(container.Roots, renderSubtree(root, effective, id))
		}
		if id == "" {
			if opts.IncludeHost {
				forest.Host = container
			}
			continue
		}
		forest.Containers = append(forest.Containers, container)
	}
	sort.Slice(forest.Containers, func(i, j int) bool { return forest.Containers[i].ID < forest.Containers[j].ID })
	return forest
}

// resolveContainer propagates a container ID down through descendants that do
// not carry one of their own.
func resolveContainer(n *node, inherited string, effective map[*node]string) {
	if n.container != "" {
		inherited = n.container
	}
	effective[n] = inherited
	for _, child := range n.children {
		resolveContainer(child, inherited, effective)
	}
}

// containerID prefers the full container ID from the pod field and falls back
// to the truncated Docker ID that Tetragon records for container processes.
func containerID(p *tetragon.Process) string {
	if id := p.GetPod().GetContainer().GetId(); id != "" {
		return id
	}
	return p.GetDocker()
}

// renderSubtree renders a node and the descendants that share its container.
// A descendant in a different container is a root of its own container instead.
func renderSubtree(n *node, effective map[*node]string, container string) *Process {
	out := renderSelf(n)
	children := make([]*node, 0, len(n.children))
	for _, child := range n.children {
		if effective[child] == container {
			children = append(children, child)
		}
	}
	sortNodes(children)
	for _, child := range children {
		out.Children = append(out.Children, renderSubtree(child, effective, container))
	}
	return out
}

func renderSelf(n *node) *Process {
	out := &Process{
		ExecID:    n.execID,
		PID:       n.pid,
		Binary:    n.binary,
		Arguments: n.arguments,
		Command:   command(n),
		CWD:       n.cwd,
		UID:       n.uid,
		Children:  []*Process{},
	}
	if !n.start.IsZero() {
		out.StartedAt = n.start.UTC().Format(time.RFC3339)
	}
	return out
}

// command renders a printable command line. Tetragon separates the executable
// from its arguments, but tolerate a dump where arguments already repeats it.
func command(n *node) string {
	switch {
	case n.arguments == "":
		return n.binary
	case n.binary == "":
		return n.arguments
	case strings.HasPrefix(n.arguments, n.binary):
		return n.arguments
	default:
		return n.binary + " " + n.arguments
	}
}

func sortNodes(nodes []*node) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if !a.start.Equal(b.start) {
			if a.start.IsZero() {
				return false
			}
			if b.start.IsZero() {
				return true
			}
			return a.start.Before(b.start)
		}
		return a.pid < b.pid
	})
}

func removeNode(nodes []*node, target *node) []*node {
	out := nodes[:0]
	for _, n := range nodes {
		if n != target {
			out = append(out, n)
		}
	}
	return out
}
