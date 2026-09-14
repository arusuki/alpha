package process

import (
	"testing"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var epoch = time.Unix(1700000000, 0)

func proc(execID, parentExec, binary, arguments, container string, pid uint32, offset time.Duration) *tetragon.Process {
	p := &tetragon.Process{
		ExecId:       execID,
		Pid:          wrapperspb.UInt32(pid),
		Binary:       binary,
		Arguments:    arguments,
		ParentExecId: parentExec,
		StartTime:    timestamppb.New(epoch.Add(offset)),
	}
	if container != "" {
		p.Docker = container
	}
	return p
}

func execEvent(p *tetragon.Process) *tetragon.GetEventsResponse {
	return &tetragon.GetEventsResponse{Event: &tetragon.GetEventsResponse_ProcessExec{ProcessExec: &tetragon.ProcessExec{Process: p}}}
}

func exitEvent(p *tetragon.Process) *tetragon.GetEventsResponse {
	return &tetragon.GetEventsResponse{Event: &tetragon.GetEventsResponse_ProcessExit{ProcessExit: &tetragon.ProcessExit{Process: p}}}
}

func onlyContainer(t *testing.T, forest *Forest, id string) *Container {
	t.Helper()
	for _, container := range forest.Containers {
		if container.ID == id {
			return container
		}
	}
	t.Fatalf("container %q missing from forest with %d containers", id, len(forest.Containers))
	return nil
}

// A child can be observed before its parent; it must be attached once the
// parent shows up rather than left as a second root.
func TestBuilderAttachesOutOfOrderChildren(t *testing.T) {
	builder := NewBuilder()
	parent := proc("parent", "", "/bin/bash", "-l", "abc123", 100, 0)
	child := proc("child", "parent", "/bin/sh", "-c sleep 10", "abc123", 200, time.Second)
	builder.Apply(execEvent(child))
	builder.Apply(execEvent(parent))

	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if container.ProcessCount != 2 {
		t.Fatalf("process count = %d, want 2", container.ProcessCount)
	}
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "parent" {
		t.Fatalf("roots = %+v, want a single parent root", container.Roots)
	}
	root := container.Roots[0]
	if root.Command != "/bin/bash -l" {
		t.Fatalf("command = %q, want %q", root.Command, "/bin/bash -l")
	}
	if len(root.Children) != 1 || root.Children[0].ExecID != "child" {
		t.Fatalf("children = %+v, want the out-of-order child attached", root.Children)
	}
	if root.Children[0].Command != "/bin/sh -c sleep 10" {
		t.Fatalf("child command = %q", root.Children[0].Command)
	}
}

// When an intermediate process exits, its children must move up to the nearest
// surviving ancestor instead of disappearing with it.
func TestExitSplicesChildrenToGrandparent(t *testing.T) {
	builder := NewBuilder()
	grandparent := proc("grandparent", "", "/bin/bash", "", "abc123", 100, 0)
	parent := proc("parent", "grandparent", "/bin/sh", "-c make", "abc123", 200, time.Second)
	child := proc("child", "parent", "/usr/bin/cc", "-c foo.c", "abc123", 300, 2*time.Second)
	builder.Apply(execEvent(grandparent))
	builder.Apply(execEvent(parent))
	builder.Apply(execEvent(child))
	builder.Apply(exitEvent(parent))

	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if container.ProcessCount != 2 {
		t.Fatalf("process count = %d, want 2 after the parent exited", container.ProcessCount)
	}
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "grandparent" {
		t.Fatalf("roots = %+v, want the grandparent alone", container.Roots)
	}
	grandchildren := container.Roots[0].Children
	if len(grandchildren) != 1 || grandchildren[0].ExecID != "child" {
		t.Fatalf("children = %+v, want the orphaned child spliced up", grandchildren)
	}
}

// Exiting the last process removes its container from the forest entirely.
func TestExitRemovesEmptyContainer(t *testing.T) {
	builder := NewBuilder()
	only := proc("init", "", "/sbin/init", "", "abc123", 1, 0)
	builder.Apply(execEvent(only))
	builder.Apply(exitEvent(only))
	if forest := builder.Snapshot(Options{}); len(forest.Containers) != 0 || builder.Len() != 0 {
		t.Fatalf("forest = %+v, want empty", forest)
	}
}

// Processes without a container ID are grouped separately and only exported on
// request, so container-focused output stays clean.
func TestSnapshotSeparatesHostProcesses(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("init", "", "/sbin/init", "", "abc123", 1, 0)))
	builder.Apply(execEvent(proc("sshd", "", "/usr/sbin/sshd", "-D", "", 900, 0)))

	excluded := builder.Snapshot(Options{})
	if len(excluded.Containers) != 1 || excluded.Host != nil {
		t.Fatalf("containers = %d, host = %v; want host excluded", len(excluded.Containers), excluded.Host)
	}
	included := builder.Snapshot(Options{IncludeHost: true})
	if included.Host == nil || included.Host.ProcessCount != 1 || included.Host.Roots[0].ExecID != "sshd" {
		t.Fatalf("host = %+v, want sshd under host processes", included.Host)
	}
}

// Tetragon records the host-side runtime (containerd-shim, runc) as the parent
// of a container's init process. Container processes must stay in their own
// container rather than following that host ancestor into the host bucket.
func TestContainerProcessesStayWithTheirContainer(t *testing.T) {
	builder := NewBuilder()
	shim := proc("shim", "", "/usr/bin/containerd-shim", "", "", 50, 0)
	init := proc("init", "shim", "/usr/bin/python3", "train.py", "abc123", 1, time.Second)
	child := proc("child", "init", "/bin/sh", "-c run", "abc123", 42, 2*time.Second)
	builder.Apply(execEvent(shim))
	builder.Apply(execEvent(init))
	builder.Apply(execEvent(child))

	forest := builder.Snapshot(Options{IncludeHost: true})
	container := onlyContainer(t, forest, "abc123")
	if container.ProcessCount != 2 {
		t.Fatalf("container process count = %d, want 2", container.ProcessCount)
	}
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "init" {
		t.Fatalf("container roots = %+v, want the init process", container.Roots)
	}
	if len(container.Roots[0].Children) != 1 || container.Roots[0].Children[0].ExecID != "child" {
		t.Fatalf("container children = %+v, want the child", container.Roots[0].Children)
	}
	if forest.Host == nil || forest.Host.ProcessCount != 1 || forest.Host.Roots[0].ExecID != "shim" {
		t.Fatalf("host = %+v, want only containerd-shim", forest.Host)
	}
	if len(forest.Host.Roots[0].Children) != 0 {
		t.Fatalf("host root has %d children, want the container process excluded", len(forest.Host.Roots[0].Children))
	}
}

// Each container must be reported exactly once no matter which process the
// snapshot happens to visit first; a child visited before its root used to
// create a second bucket for the same container.
func TestSnapshotEmitsEachContainerOnce(t *testing.T) {
	builder := NewBuilder()
	ids := []string{"aaa", "bbb", "ccc"}
	for _, id := range ids {
		root := proc(id+"-root", "", "/sbin/init", "", id, 1, 0)
		child := proc(id+"-child", id+"-root", "/bin/sh", "-c run", id, 2, time.Second)
		builder.Apply(execEvent(child))
		builder.Apply(execEvent(root))
	}
	// Process map iteration order is randomized, so repeat the snapshot to
	// make an order-dependent bucket bug show up reliably.
	for i := 0; i < 200; i++ {
		forest := builder.Snapshot(Options{})
		if len(forest.Containers) != len(ids) {
			t.Fatalf("containers = %d, want %d", len(forest.Containers), len(ids))
		}
		seen := map[string]bool{}
		for _, container := range forest.Containers {
			if seen[container.ID] {
				t.Fatalf("container %q reported more than once", container.ID)
			}
			seen[container.ID] = true
			if container.ProcessCount != 2 {
				t.Fatalf("container %q process count = %d, want 2", container.ID, container.ProcessCount)
			}
		}
	}
}

// Command rendering tolerates a dump that already repeats the executable.
func TestCommandRendering(t *testing.T) {
	cases := []struct{ binary, arguments, want string }{
		{"/bin/ls", "-la /tmp", "/bin/ls -la /tmp"},
		{"/bin/ls", "", "/bin/ls"},
		{"", "/bin/ls", "/bin/ls"},
		{"/bin/ls", "/bin/ls -la", "/bin/ls -la"},
	}
	for _, c := range cases {
		if got := command(&node{binary: c.binary, arguments: c.arguments}); got != c.want {
			t.Fatalf("command(%q, %q) = %q, want %q", c.binary, c.arguments, got, c.want)
		}
	}
}

// Reconciliation restores what the process cache knows but the forest lost, and
// does not count a process younger than the grace period as a ghost: the cache
// lags the event stream, so a just-executed process is expected to be missing.
func TestReconcileRestoresAndFiltersYoungGhosts(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("old", "", "/bin/old", "", "abc123", 10, 0)))
	builder.Apply(execEvent(proc("young", "", "/bin/young", "", "abc123", 11, time.Since(epoch))))

	cache := []*tetragon.Process{
		proc("old", "", "/bin/old", "", "abc123", 10, 0),
		proc("lost", "", "/bin/lost", "", "abc123", 12, 0),
	}
	drift := builder.Reconcile(cache, time.Hour, nil)
	if drift.Added != 1 || drift.Matched != 1 || drift.Ghosts != 0 {
		t.Fatalf("drift = %+v, want the lost process added and the young one not counted", drift)
	}
	if builder.Len() != 3 {
		t.Fatalf("builder length = %d, want the lost process restored", builder.Len())
	}

	// With no grace, both processes the cache omits are old enough to count.
	drift = builder.Reconcile([]*tetragon.Process{proc("old", "", "/bin/old", "", "abc123", 10, 0)}, 0, nil)
	if drift.Ghosts != 2 || drift.Matched != 1 || drift.Added != 0 || drift.Removed != 0 {
		t.Fatalf("drift = %+v, want both omitted processes counted as ghosts", drift)
	}
	if builder.Len() != 3 {
		t.Fatalf("builder length = %d, want a first miss to keep every process", builder.Len())
	}
}

// fakeProber is a Prober whose answer the test supplies.
type fakeProber func(pid uint32, start time.Time) bool

func (f fakeProber) Alive(pid uint32, start time.Time) bool { return f(pid, start) }

// A process the cache keeps omitting is treated as exited without an exit event
// reaching us, but only after the miss repeats: one sample is not enough. With
// no prober this is the whole decision, which is what a host without a readable
// procfs falls back to.
func TestReconcileDropsOnlyAfterRepeatedMisses(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("gone", "", "/bin/gone", "", "abc123", 10, 0)))

	if drift := builder.Reconcile(nil, 0, nil); drift.Ghosts != 1 || drift.Removed != 0 {
		t.Fatalf("first miss: drift = %+v, want the process kept and counted", drift)
	}
	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want the process kept after one miss", builder.Len())
	}
	if drift := builder.Reconcile(nil, 0, nil); drift.Removed != 1 || drift.Ghosts != 0 {
		t.Fatalf("second miss: drift = %+v, want the process dropped", drift)
	}
	if builder.Len() != 0 {
		t.Fatalf("builder length = %d, want the process dropped after two misses", builder.Len())
	}
}

// A process the cache lists again is matched and its misses are forgotten: a
// dump that lagged the event stream must not leave a mark.
func TestReconcileRestoresAfterATransientMiss(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("alive", "", "/bin/alive", "", "abc123", 10, 0)))

	if drift := builder.Reconcile(nil, 0, nil); drift.Ghosts != 1 {
		t.Fatalf("drift = %+v, want the transient miss counted", drift)
	}
	cache := []*tetragon.Process{proc("alive", "", "/bin/alive", "", "abc123", 10, 0)}
	drift := builder.Reconcile(cache, 0, nil)
	if drift.Ghosts != 0 || drift.Removed != 0 || drift.Matched != 1 {
		t.Fatalf("drift = %+v, want the live process matched, not dropped", drift)
	}
	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want the live process kept", builder.Len())
	}
}

// The prober is a backstop for the drop, not a per-miss poll: it is consulted
// only once the miss counter is ready to remove the process, and it is asked
// about the process the node describes.
func TestReconcileProbesOnlyWhenADropIsDue(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("gone", "", "/bin/gone", "", "abc123", 42, 0)))
	var gotPID uint32
	var gotStart time.Time
	calls := 0
	prober := fakeProber(func(pid uint32, start time.Time) bool {
		calls++
		gotPID, gotStart = pid, start
		return true
	})

	if drift := builder.Reconcile(nil, 0, prober); drift.Ghosts != 1 || calls != 0 {
		t.Fatalf("first miss: drift = %+v, calls = %d; want one ghost and no probe", drift, calls)
	}
	if drift := builder.Reconcile(nil, 0, prober); drift.Evicted != 1 || calls != 1 {
		t.Fatalf("second miss: drift = %+v, calls = %d; want one probe and the process kept", drift, calls)
	}
	if gotPID != 42 || !gotStart.Equal(epoch) {
		t.Fatalf("probed pid = %d, start = %v; want the node's pid 42 started at %v", gotPID, gotStart, epoch)
	}
}

// The agent's process cache is capacity-bounded and evicts running processes.
// An evicted process is absent from every later dump, so the miss counter would
// delete it for good and nothing could restore it. The prober must veto that,
// however many reconciliations follow.
func TestReconcileKeepsProcessTheCacheEvicted(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("daemon", "", "/bin/daemon", "", "abc123", 10, 0)))
	prober := fakeProber(func(uint32, time.Time) bool { return true })

	for i := 0; i < 4; i++ {
		drift := builder.Reconcile(nil, 0, prober)
		if i == 0 {
			// The first miss is still indistinguishable from a lagging dump.
			if drift.Ghosts != 1 || drift.Evicted != 0 {
				t.Fatalf("first miss: drift = %+v, want one ghost and no eviction yet", drift)
			}
			continue
		}
		if drift.Evicted != 1 || drift.Removed != 0 || drift.Ghosts != 0 {
			t.Fatalf("miss %d: drift = %+v, want the evicted process kept", i+1, drift)
		}
		if builder.Len() != 1 {
			t.Fatalf("miss %d: builder length = %d, want the running process kept", i+1, builder.Len())
		}
	}
}

// Once the process really exits, the probe stops confirming it and the miss
// counter removes it, so the veto cannot turn into a permanent ghost.
func TestReconcileDropsEvictedProcessAfterItExits(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("daemon", "", "/bin/daemon", "", "abc123", 10, 0)))
	alive := true
	prober := fakeProber(func(uint32, time.Time) bool { return alive })

	builder.Reconcile(nil, 0, prober)
	builder.Reconcile(nil, 0, prober)
	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want the running process kept", builder.Len())
	}

	alive = false
	if drift := builder.Reconcile(nil, 0, prober); drift.Removed != 1 || drift.Evicted != 0 {
		t.Fatalf("drift = %+v, want the process dropped once it exited", drift)
	}
	if builder.Len() != 0 {
		t.Fatalf("builder length = %d, want the process dropped once it exited", builder.Len())
	}
}

// An in-place exec reports a new exec ID for the same PID and start time, with
// no exit event for the image it replaced. The node must be re-labelled to the
// new image rather than duplicated, so the forest never shows one process twice.
func TestExecReplacesSupersededImage(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("old-image", "", "/usr/bin/sh", "", "abc123", 100, 0)))
	builder.Apply(execEvent(proc("new-image", "", "/bin/bash", "-l", "abc123", 100, 0)))

	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want one node for the replaced image", builder.Len())
	}
	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if container.ProcessCount != 1 || len(container.Roots) != 1 {
		t.Fatalf("container = %+v, want a single root", container)
	}
	if root := container.Roots[0]; root.ExecID != "new-image" || root.Command != "/bin/bash -l" {
		t.Fatalf("root = %+v, want the new image", root)
	}
}

// Re-labelling must keep the task's place in the tree: a child forked before
// the exec stays attached to the process that is still its parent.
func TestExecReplacesSupersededImageKeepingChildren(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("worker", "", "/usr/bin/sh", "", "abc123", 100, 0)))
	builder.Apply(execEvent(proc("child", "worker", "/bin/sleep", "10", "abc123", 200, time.Second)))
	builder.Apply(execEvent(proc("worker-2", "", "/bin/bash", "", "abc123", 100, 0)))

	if builder.Len() != 2 {
		t.Fatalf("builder length = %d, want the exec to replace rather than add", builder.Len())
	}
	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "worker-2" {
		t.Fatalf("roots = %+v, want the re-labelled process alone", container.Roots)
	}
	if children := container.Roots[0].Children; len(children) != 1 || children[0].ExecID != "child" {
		t.Fatalf("children = %+v, want the pre-exec child still attached", children)
	}
}

// If the exec event itself was lost, the process cache still lists the current
// image, so reconciliation re-labels the stale node instead of leaving the old
// binary in the forest or putting two nodes on the PID.
func TestReconcileRepairsMissingExecEvent(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("old-image", "", "/usr/bin/sh", "", "abc123", 100, 0)))

	cache := []*tetragon.Process{proc("new-image", "", "/bin/bash", "-l", "abc123", 100, 0)}
	drift := builder.Reconcile(cache, 0, nil)
	if drift.Added != 0 || drift.Matched != 1 {
		t.Fatalf("drift = %+v, want the current image matched, not added", drift)
	}
	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want one node", builder.Len())
	}
	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "new-image" {
		t.Fatalf("roots = %+v, want the current image", container.Roots)
	}
}

// A PID the kernel recycled starts a different task with a later start time, so
// it is not an exec of the node already on that PID and must not replace it.
func TestReconcileDoesNotMergeRecycledPID(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("old-task", "", "/bin/old", "", "abc123", 100, 0)))
	builder.Apply(execEvent(proc("new-task", "", "/bin/new", "", "abc123", 100, time.Hour)))
	if builder.Len() != 2 {
		t.Fatalf("builder length = %d, want the recycled PID kept separate", builder.Len())
	}

	// The probe cannot tell the two apart on one PID, so the stale task is left
	// to the miss counter rather than kept alive by the veto.
	prober := fakeProber(func(uint32, time.Time) bool { return true })
	cache := []*tetragon.Process{proc("new-task", "", "/bin/new", "", "abc123", 100, time.Hour)}
	builder.Reconcile(cache, 0, prober)
	drift := builder.Reconcile(cache, 0, prober)
	if drift.Removed != 1 || drift.Evicted != 0 || drift.Matched != 1 {
		t.Fatalf("drift = %+v, want the stale task dropped and the current one matched", drift)
	}
	if builder.Len() != 1 {
		t.Fatalf("builder length = %d, want only the current task", builder.Len())
	}
}

// The agent's capacity-bounded cache can evict a process that re-exec'd in
// place. Because the exec replaced the old node rather than leaving a second
// one on the PID, the prober is no longer withheld, so the evicted live image
// survives. Before that the lingering old image made the PID ambiguous and the
// live image was deleted for good.
func TestReconcileProtectsEvictedImageOfReExec(t *testing.T) {
	builder := NewBuilder()
	builder.Apply(execEvent(proc("old-image", "", "/usr/bin/sh", "", "abc123", 100, 0)))
	builder.Apply(execEvent(proc("new-image", "", "/bin/bash", "", "abc123", 100, 0)))
	prober := fakeProber(func(uint32, time.Time) bool { return true })

	for i := 0; i < 4; i++ {
		drift := builder.Reconcile(nil, 0, prober)
		if i == 0 {
			if drift.Ghosts != 1 || drift.Evicted != 0 {
				t.Fatalf("first miss: drift = %+v, want one ghost and no eviction yet", drift)
			}
			continue
		}
		if drift.Evicted != 1 || drift.Removed != 0 {
			t.Fatalf("miss %d: drift = %+v, want the evicted live image kept", i+1, drift)
		}
		if builder.Len() != 1 {
			t.Fatalf("miss %d: builder length = %d, want the live image kept", i+1, builder.Len())
		}
	}
}
