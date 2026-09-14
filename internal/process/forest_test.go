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
