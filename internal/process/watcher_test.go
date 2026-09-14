package process

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// fakeRound is what one stream "delivery" carries: some events, then either an
// error that ends the stream or nothing, which keeps it open.
type fakeRound struct {
	events []*tetragon.GetEventsResponse
	err    error
}

// fakeSource is a controllable Source. Bootstrap replays one configured listing
// per call and repeats the last one, so a reconnect or a reconciliation is
// observable; with no listings at all it behaves like a source that cannot
// enumerate processes.
type fakeSource struct {
	mu         sync.Mutex
	listings   [][]*tetragon.Process
	bootstrapN int
	delivered  int
	rounds     chan fakeRound
	gate       chan struct{}
	failure    error
}

func newFakeSource(listings ...[]*tetragon.Process) *fakeSource {
	return &fakeSource{listings: listings, rounds: make(chan fakeRound, 64)}
}

// setGate makes the next Bootstrap call block until the returned channel is
// closed, which is how a test holds the cold-start window open.
func (f *fakeSource) setGate(gate chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = gate
}

// setListing replaces the process cache every later Bootstrap call reports.
func (f *fakeSource) setListing(processes ...*tetragon.Process) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listings = [][]*tetragon.Process{processes}
}

func (f *fakeSource) setBootstrapError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = err
}

func (f *fakeSource) Bootstrap(ctx context.Context) ([]*tetragon.Process, error) {
	f.mu.Lock()
	if f.listings == nil {
		f.mu.Unlock()
		return nil, ErrNoBootstrap
	}
	index := f.bootstrapN
	f.bootstrapN++
	if index >= len(f.listings) {
		index = len(f.listings) - 1
	}
	listing, gate, failure := f.listings[index], f.gate, f.failure
	f.gate = nil // only the next call waits
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failure != nil {
		return nil, failure
	}
	return listing, nil
}

func (f *fakeSource) bootstrapCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bootstrapN
}

func (f *fakeSource) deliveredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivered
}

// Stream delivers rounds until one carries an error; with none pending it stays
// open until the context is cancelled, like a live subscription.
func (f *fakeSource) Stream(ctx context.Context, handle func(*tetragon.GetEventsResponse) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case round := <-f.rounds:
			for _, event := range round.events {
				if err := handle(event); err != nil {
					return err
				}
				f.mu.Lock()
				f.delivered++
				f.mu.Unlock()
			}
			if round.err != nil {
				return round.err
			}
		}
	}
}

func (f *fakeSource) Close() error { return nil }

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func containerOf(forest *Forest, id string) *Container {
	for _, container := range forest.Containers {
		if container.ID == id {
			return container
		}
	}
	return nil
}

func execIDs(container *Container) map[string]bool {
	ids := map[string]bool{}
	if container == nil {
		return ids
	}
	var walk func([]*Process)
	walk = func(nodes []*Process) {
		for _, n := range nodes {
			ids[n.ExecID] = true
			walk(n.Children)
		}
	}
	walk(container.Roots)
	return ids
}

// cachedProc builds a process that a process-cache dump would report. Its start
// time is far in the past, so it is old enough to be counted as a ghost. The PID
// is derived from the name, because two live processes on one host cannot share
// a PID and reconciliation reads a shared PID as a superseded exec image.
func cachedProc(execID string) *tetragon.Process {
	var pid uint32
	for i := 0; i < len(execID); i++ {
		pid = pid*31 + uint32(execID[i])
	}
	return proc(execID, "", "/bin/"+execID, "", "abc123", pid, 0)
}

// Starting the watcher must build the tree immediately from the process cache,
// before any event arrives.
func TestWatcherBuildsTreeOnStart(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{
		proc("parent", "", "/bin/bash", "-l", "abc123", 100, 0),
		proc("child", "parent", "/bin/sh", "-c sleep 10", "abc123", 200, time.Second),
	})
	watcher := NewWatcher(context.Background(), src)
	defer watcher.Close()

	waitFor(t, "bootstrapped forest", func() bool {
		forest, status := watcher.Snapshot(false)
		return status.Bootstrapped && status.Connected && containerOf(forest, "abc123") != nil
	})
	forest, status := watcher.Snapshot(false)
	container := containerOf(forest, "abc123")
	if container.ProcessCount != 2 {
		t.Fatalf("process count = %d, want 2", container.ProcessCount)
	}
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "parent" {
		t.Fatalf("roots = %+v, want a single parent root", container.Roots)
	}
	if !status.Since.Before(time.Now().Add(time.Second)) {
		t.Fatalf("status = %+v, want a start time in the past", status)
	}
}

// Subscribing happens before the process cache is dumped, so an event that
// arrives in that window must still reach the tree instead of being lost.
func TestWatcherBuffersEventsDuringBootstrap(t *testing.T) {
	gate := make(chan struct{})
	src := newFakeSource([]*tetragon.Process{cachedProc("dump")})
	src.setGate(gate)
	watcher := newWatcher(context.Background(), src, time.Hour, nil)
	defer watcher.Close()

	waitFor(t, "the process cache dump to start", func() bool { return src.bootstrapCalls() >= 1 })
	window := proc("window", "", "/bin/window", "", "abc123", 200, 0)
	src.rounds <- fakeRound{events: []*tetragon.GetEventsResponse{execEvent(window)}}
	waitFor(t, "the event to arrive while the dump is held open", func() bool { return src.deliveredCount() >= 1 })
	close(gate)

	waitFor(t, "the dump plus the buffered event", func() bool {
		forest, _ := watcher.Snapshot(false)
		ids := execIDs(containerOf(forest, "abc123"))
		return ids["dump"] && ids["window"]
	})
}

// Live events must keep updating the in-memory tree after the initial build.
func TestWatcherAppliesLiveEvents(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{})
	watcher := NewWatcher(context.Background(), src)
	defer watcher.Close()

	parent := proc("parent", "", "/bin/bash", "", "abc123", 100, 0)
	child := proc("child", "parent", "/bin/sh", "-c run", "abc123", 200, time.Second)
	src.rounds <- fakeRound{events: []*tetragon.GetEventsResponse{execEvent(parent), execEvent(child)}}
	waitFor(t, "two live processes", func() bool {
		forest, _ := watcher.Snapshot(false)
		container := containerOf(forest, "abc123")
		return container != nil && container.ProcessCount == 2
	})

	src.rounds <- fakeRound{events: []*tetragon.GetEventsResponse{exitEvent(parent)}}
	waitFor(t, "the parent exit to splice its child", func() bool {
		forest, _ := watcher.Snapshot(false)
		container := containerOf(forest, "abc123")
		return container != nil && container.ProcessCount == 1 &&
			len(container.Roots) == 1 && container.Roots[0].ExecID == "child"
	})
}

// A broken stream must reconnect and rebuild from a fresh listing rather than
// carry on with the stale tree.
func TestWatcherReconnectsAndRebuildsTree(t *testing.T) {
	src := newFakeSource(
		[]*tetragon.Process{proc("old", "", "/bin/old", "gone", "abc123", 10, 0)},
		[]*tetragon.Process{proc("new", "", "/bin/new", "here", "abc123", 20, time.Second)},
	)
	watcher := NewWatcher(context.Background(), src)
	defer watcher.Close()

	waitFor(t, "the first listing", func() bool {
		forest, _ := watcher.Snapshot(false)
		container := containerOf(forest, "abc123")
		return container != nil && len(container.Roots) == 1 && container.Roots[0].ExecID == "old"
	})

	src.rounds <- fakeRound{err: errors.New("tetragon socket closed")}
	waitFor(t, "a reconnect that re-lists the process cache", func() bool {
		forest, status := watcher.Snapshot(false)
		if status.Reconnects < 1 || src.bootstrapCalls() < 2 {
			return false
		}
		container := containerOf(forest, "abc123")
		return container != nil && len(container.Roots) == 1 && container.Roots[0].ExecID == "new"
	})
}

// A process the cache knows but the event stream never delivered must be
// restored by reconciliation, which is the only recovery path for events the
// agent itself dropped.
func TestWatcherAlignmentAddsMissedProcesses(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{cachedProc("a")})
	watcher := newWatcher(context.Background(), src, time.Hour, nil)
	defer watcher.Close()
	waitFor(t, "the initial forest", func() bool {
		forest, _ := watcher.Snapshot(false)
		return execIDs(containerOf(forest, "abc123"))["a"]
	})

	src.setListing(cachedProc("a"), cachedProc("b"))
	watcher.reconcile(context.Background())

	forest, status := watcher.Snapshot(false)
	ids := execIDs(containerOf(forest, "abc123"))
	if !ids["a"] || !ids["b"] {
		t.Fatalf("exec ids = %v, want the missed process restored", ids)
	}
	if status.Alignment == nil {
		t.Fatal("alignment was not recorded")
	}
	if status.Alignment.Added != 1 || status.Alignment.Matched != 1 || status.Alignment.Ghosts != 0 {
		t.Fatalf("alignment = %+v, want 1 added and 1 matched, no ghosts", status.Alignment)
	}
	if status.Alignment.Percent != 50 {
		t.Fatalf("percent = %v, want 50", status.Alignment.Percent)
	}
}

// A process whose exit event was lost shows up as a ghost and is dropped only
// once the cache omits it twice, so one lagging dump cannot remove it.
func TestWatcherAlignmentDropsRepeatedGhosts(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{cachedProc("a"), cachedProc("b")})
	watcher := newWatcher(context.Background(), src, time.Hour, nil)
	defer watcher.Close()
	waitFor(t, "both processes", func() bool {
		forest, _ := watcher.Snapshot(false)
		return len(execIDs(containerOf(forest, "abc123"))) == 2
	})

	src.setListing(cachedProc("a"))
	watcher.reconcile(context.Background())

	forest, status := watcher.Snapshot(false)
	if ids := execIDs(containerOf(forest, "abc123")); !ids["a"] || !ids["b"] {
		t.Fatalf("exec ids = %v, want the first miss to keep the process", ids)
	}
	if status.Alignment.Ghosts != 1 || status.Alignment.Removed != 0 || status.Alignment.Matched != 1 {
		t.Fatalf("alignment = %+v, want one ghost pending removal and one match", status.Alignment)
	}
	if status.Alignment.Percent != 50 {
		t.Fatalf("percent = %v, want 50", status.Alignment.Percent)
	}

	watcher.reconcile(context.Background())
	forest, status = watcher.Snapshot(false)
	if ids := execIDs(containerOf(forest, "abc123")); !ids["a"] || ids["b"] {
		t.Fatalf("exec ids = %v, want the repeated ghost dropped", ids)
	}
	if status.Alignment.Removed != 1 || status.Alignment.Ghosts != 0 {
		t.Fatalf("alignment = %+v, want one removal", status.Alignment)
	}
	if status.Alignment.Percent != 100 {
		t.Fatalf("percent = %v, want 100 once both agree", status.Alignment.Percent)
	}
}

// The agent's process cache is capacity-bounded and evicts running processes.
// Such a process is missing from every later dump, so without the probe the
// reconciliation would delete a process that is still running, for good. It
// must survive, and the eviction must be reported so an undersized agent cache
// is visible rather than silent.
func TestWatcherAlignmentKeepsEvictedProcess(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{cachedProc("a"), cachedProc("b")})
	prober := fakeProber(func(uint32, time.Time) bool { return true })
	watcher := newWatcher(context.Background(), src, time.Hour, prober)
	defer watcher.Close()
	waitFor(t, "both processes", func() bool {
		forest, _ := watcher.Snapshot(false)
		return len(execIDs(containerOf(forest, "abc123"))) == 2
	})

	// The cache now omits "b", which was evicted rather than exited.
	src.setListing(cachedProc("a"))
	watcher.reconcile(context.Background())
	watcher.reconcile(context.Background())

	forest, status := watcher.Snapshot(false)
	if ids := execIDs(containerOf(forest, "abc123")); !ids["a"] || !ids["b"] {
		t.Fatalf("exec ids = %v, want the evicted process kept", ids)
	}
	if status.Alignment.Evicted != 1 || status.Alignment.Removed != 0 || status.Alignment.Matched != 1 {
		t.Fatalf("alignment = %+v, want one eviction and one match, no removal", status.Alignment)
	}
	if status.Alignment.Percent != 50 {
		t.Fatalf("percent = %v, want 50 while the cache lags the forest", status.Alignment.Percent)
	}
}

// The resident watcher must probe liveness by default, or the eviction fix
// would be wired up nowhere outside the tests.
func TestNewWatcherProbesLivenessByDefault(t *testing.T) {
	watcher := NewWatcher(context.Background(), newFakeSource([]*tetragon.Process{}))
	defer watcher.Close()
	if _, ok := watcher.prober.(*ProcProber); !ok {
		t.Fatalf("prober = %T, want *ProcProber", watcher.prober)
	}
}

// A failed reconciliation is recorded but must not disturb collection.
func TestWatcherAlignmentFailureIsNotFatal(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{cachedProc("a")})
	watcher := newWatcher(context.Background(), src, time.Hour, nil)
	defer watcher.Close()
	waitFor(t, "the initial forest", func() bool {
		forest, _ := watcher.Snapshot(false)
		return execIDs(containerOf(forest, "abc123"))["a"]
	})

	src.setBootstrapError(errors.New("tetragon unreachable"))
	watcher.reconcile(context.Background())

	_, status := watcher.Snapshot(false)
	if status.Alignment == nil || status.Alignment.Error == "" {
		t.Fatalf("alignment = %+v, want the failure recorded", status.Alignment)
	}
	if !status.Connected {
		t.Fatal("a failed alignment must not report the watcher as disconnected")
	}
}

// A source that cannot enumerate processes has nothing to align against, so it
// must not be reported as a reconciliation failure.
func TestWatcherAlignmentSkipsSourcesWithoutCache(t *testing.T) {
	src := newFakeSource()
	watcher := newWatcher(context.Background(), src, time.Hour, nil)
	defer watcher.Close()

	watcher.reconcile(context.Background())
	if _, status := watcher.Snapshot(false); status.Alignment != nil {
		t.Fatalf("alignment = %+v, want none without a process cache", status.Alignment)
	}
}

// Readers snapshot while the stream applies events and reconciliation rewrites
// the status; run under -race to prove the watcher's lock covers both.
func TestWatcherSnapshotIsRaceFree(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{cachedProc("boot")})
	watcher := newWatcher(context.Background(), src, 5*time.Millisecond, nil)
	defer watcher.Close()

	var producers sync.WaitGroup
	producers.Add(1)
	go func() {
		defer producers.Done()
		for i := 0; i < 300; i++ {
			id := fmt.Sprintf("proc-%d", i)
			src.rounds <- fakeRound{events: []*tetragon.GetEventsResponse{
				execEvent(proc(id, "", "/bin/sh", "-c run", "abc123", uint32(i+2), time.Duration(i)*time.Millisecond)),
			}}
		}
	}()
	for i := 0; i < 300; i++ {
		watcher.Snapshot(i%2 == 0)
	}
	producers.Wait()
}

// The resident path must rebuild exactly the forest the offline replay test
// asserts, so the two entry points cannot drift apart.
func TestWatcherMatchesReplayFixture(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{})
	src.rounds <- fakeRound{events: loadFixtureEvents(t, "../../tests/fixtures/tetragon-events.jsonl")}
	watcher := NewWatcher(context.Background(), src)
	defer watcher.Close()

	waitFor(t, "the fixture forest", func() bool {
		forest, _ := watcher.Snapshot(true)
		totals := map[string]int{}
		for _, container := range forest.Containers {
			totals[container.ID] = container.ProcessCount
		}
		return len(forest.Containers) == 2 && totals["3f2a9c8b1d4e"] == 3 && totals["b7c1e0f2a9d3"] == 2 &&
			forest.Host != nil && forest.Host.ProcessCount == 2
	})
}

func loadFixtureEvents(t *testing.T, path string) []*tetragon.GetEventsResponse {
	t.Helper()
	source, err := OpenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var events []*tetragon.GetEventsResponse
	if err := source.Stream(context.Background(), func(resp *tetragon.GetEventsResponse) error {
		events = append(events, resp)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func dispatch(handler *Handler, method, target string) (int, any, error) {
	return handler.Dispatch(httptest.NewRecorder(), httptest.NewRequest(method, target, nil), platform.User{Role: "viewer"})
}

// Without a Tetragon agent the route reports the feature as unavailable rather
// than serving an empty forest, and unknown paths or methods stay 404.
func TestProcessHandlerUnavailableWithoutWatcher(t *testing.T) {
	handler := NewHandler(nil)
	for _, target := range []string{"/api/process/forest", "/api/process/unknown"} {
		_, _, err := dispatch(handler, "GET", target)
		var apiError *httpapi.Error
		want := 503
		if target == "/api/process/unknown" {
			want = 404
		}
		if !errors.As(err, &apiError) || apiError.Status != want {
			t.Fatalf("%s err = %v, want status %d", target, err, want)
		}
	}
	if _, _, err := dispatch(handler, "POST", "/api/process/forest"); err == nil {
		t.Fatal("expected a non-GET request to be rejected")
	}
}

func TestProcessHandlerServesForest(t *testing.T) {
	src := newFakeSource([]*tetragon.Process{proc("init", "", "/sbin/init", "", "abc123", 1, 0)})
	watcher := NewWatcher(context.Background(), src)
	defer watcher.Close()
	waitFor(t, "a bootstrapped forest", func() bool {
		_, status := watcher.Snapshot(false)
		return status.Bootstrapped
	})

	handler := NewHandler(watcher)
	status, value, err := dispatch(handler, "GET", "/api/process/forest")
	if err != nil || status != 200 {
		t.Fatalf("status = %d, err = %v; want 200", status, err)
	}
	body, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value = %T, want a map", value)
	}
	forest, ok := body["forest"].(*Forest)
	if !ok || len(forest.Containers) != 1 || forest.Containers[0].ProcessCount != 1 {
		t.Fatalf("forest = %+v, want one container with one process", body["forest"])
	}
	if _, ok := body["status"].(Status); !ok {
		t.Fatalf("status = %T, want a process.Status", body["status"])
	}
}
