package process

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
)

const (
	// Reconnect backoff bounds. A session that stayed up at least maxBackoff is
	// treated as healthy, so the next failure starts over at initialBackoff.
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second

	// eventBuffer absorbs the events that arrive while the process cache is
	// being dumped. A full buffer applies backpressure to the stream rather
	// than dropping, since dropping would recreate the loss it exists to cover.
	eventBuffer = 8192

	// alignInterval is how often the forest is reconciled with the agent's
	// process cache, the only recovery path for events the agent dropped.
	alignInterval = 30 * time.Second
)

// Alignment estimates how closely the tracked forest matches the agent's
// process cache at the last reconciliation. It is a best-effort number: the
// agent may drop events from its own buffer and the cache is only a sample.
// Evicted counts processes the cache no longer lists but that are still
// running, which is what a capacity-bounded cache looks like from here; a
// nonzero, growing value means the agent's process cache is too small.
type Alignment struct {
	At      time.Time `json:"at"`
	Matched int       `json:"matched"`
	Added   int       `json:"added"`
	Ghosts  int       `json:"ghosts"`
	Evicted int       `json:"evicted,omitempty"`
	Removed int       `json:"removed,omitempty"`
	Percent float64   `json:"aligned_percent"`
	Error   string    `json:"error,omitempty"`
}

// Status reports how the resident watcher is doing, so a reader can tell an
// empty forest from a disconnected one and judge how current it is.
type Status struct {
	Connected    bool       `json:"connected"`
	Bootstrapped bool       `json:"bootstrapped"`
	Error        string     `json:"error,omitempty"`
	Since        time.Time  `json:"since"`
	Reconnects   int        `json:"reconnects"`
	Alignment    *Alignment `json:"alignment,omitempty"`
}

// Watcher keeps a container process forest in memory for the lifetime of the
// process: each session subscribes to the event stream before dumping the
// agent's process cache, then replays the buffered events on top. Nothing is
// persisted.
//
// The Builder it owns is not safe for concurrent use, so every access goes
// through mu: the loop goroutine holds the write lock only while applying one
// event or reconciling, and Snapshot readers hold the read lock while rendering
// a copy.
type Watcher struct {
	src           Source
	prober        Prober
	started       time.Time
	alignInterval time.Duration
	mu            sync.RWMutex
	builder       *Builder
	status        Status
	cancel        context.CancelFunc
	stopped       chan struct{}
	closeOnce     sync.Once
}

// NewWatcher starts collecting from src in the background. Cancelling parent,
// or calling Close, stops it.
func NewWatcher(parent context.Context, src Source) *Watcher {
	return newWatcher(parent, src, alignInterval, NewProcProber())
}

// newWatcher is NewWatcher with an explicit reconciliation period and liveness
// prober, so tests can drive alignment without waiting out the default and
// without depending on the host's procfs.
func newWatcher(parent context.Context, src Source, interval time.Duration, prober Prober) *Watcher {
	ctx, cancel := context.WithCancel(parent)
	started := time.Now()
	w := &Watcher{
		src:           src,
		prober:        prober,
		started:       started,
		alignInterval: interval,
		builder:       NewBuilder(),
		status:        Status{Since: started},
		cancel:        cancel,
		stopped:       make(chan struct{}),
	}
	go w.loop(ctx)
	return w
}

// loop runs sessions until ctx is cancelled, backing off between reconnects.
func (w *Watcher) loop(ctx context.Context) {
	defer close(w.stopped)
	backoff := initialBackoff
	for {
		attempt := time.Now()
		w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(attempt) >= maxBackoff {
			backoff = initialBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// session subscribes before dumping the process cache, so the events that occur
// in between are buffered instead of lost, then applies them on top of the dump.
func (w *Watcher) session(ctx context.Context) {
	events := make(chan *tetragon.GetEventsResponse, eventBuffer)
	streamErr := make(chan error, 1)
	// Subscribe first, but only enqueue here: the builder stays owned by this
	// goroutine, since a concurrent Apply and ObserveAll would race.
	go func() {
		_, err := streamInto(ctx, w.src, func(resp *tetragon.GetEventsResponse) {
			select {
			case events <- resp:
			case <-ctx.Done():
			}
		}, Stats{})
		streamErr <- err
	}()

	builder := NewBuilder()
	stats := bootstrapInto(ctx, w.src, builder)
	if ctx.Err() != nil {
		return
	}
	w.publish(builder, stats)

	ticker := time.NewTicker(w.alignInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-streamErr:
			w.disconnected(err)
			return
		case resp := <-events:
			w.mu.Lock()
			builder.Apply(resp)
			w.mu.Unlock()
		case <-ticker.C:
			w.reconcile(ctx)
		}
	}
}

// publish swaps in a freshly built tree and records the bootstrap result. A
// failed bootstrap means the agent was unreachable, so the stream is reported
// as disconnected until the next session succeeds. The alignment estimate
// describes the tree that was just replaced, so it is dropped here.
func (w *Watcher) publish(builder *Builder, stats Stats) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.builder = builder
	w.status.Bootstrapped = stats.BootstrapOK
	w.status.Connected = stats.BootstrapError == ""
	w.status.Error = stats.BootstrapError
	w.status.Alignment = nil
}

// disconnected records why a session ended. A clean end of stream is still a
// disconnect worth reconnecting from.
func (w *Watcher) disconnected(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.Connected = false
	w.status.Reconnects++
	if err == nil {
		w.status.Error = "事件流已结束，正在重连"
		return
	}
	w.status.Error = err.Error()
}

// reconcile aligns the forest with a fresh dump of the agent's process cache
// and records how well they agree. A failed or missing dump only refreshes the
// estimate; only a successful dump can drop a process, and only once the
// liveness prober fails to confirm it is still running.
func (w *Watcher) reconcile(ctx context.Context) {
	live, err := w.src.Bootstrap(ctx)
	if errors.Is(err, ErrNoBootstrap) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.status.Alignment = &Alignment{At: time.Now(), Error: err.Error()}
		return
	}
	drift := w.builder.Reconcile(live, w.alignInterval, w.prober)
	w.status.Alignment = &Alignment{
		At:      time.Now(),
		Matched: drift.Matched,
		Added:   drift.Added,
		Ghosts:  drift.Ghosts,
		Evicted: drift.Evicted,
		Removed: drift.Removed,
		Percent: alignedPercent(drift),
	}
}

// alignedPercent is the share of processes the forest and the cache agree on,
// out of everything either of them still knows about, so both a missing process
// and a lingering one lower it. Processes just dropped are in neither, since
// they are gone from the forest and were already absent from the cache. An
// evicted process counts against the total because the forest knows it and the
// cache does not: it is a real disagreement, and the separate evicted count
// attributes it to the agent's cache rather than to our tracking.
func alignedPercent(drift Drift) float64 {
	total := drift.Matched + drift.Added + drift.Ghosts + drift.Evicted
	if total == 0 {
		return 100
	}
	return math.Round(1000*float64(drift.Matched)/float64(total)) / 10
}

// Snapshot renders the current forest and the watcher status. Children are
// ordered and every container appears once, since it reuses Builder.Snapshot.
func (w *Watcher) Snapshot(includeHost bool) (*Forest, Status) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	forest := w.builder.Snapshot(Options{
		IncludeHost:  includeHost,
		CapturedAt:   time.Now(),
		Duration:     time.Since(w.started),
		Bootstrapped: w.status.Bootstrapped,
	})
	return forest, w.status
}

// Close stops the loop and releases the source. It is safe to call twice.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		w.cancel()
		<-w.stopped
		_ = w.src.Close()
	})
}
