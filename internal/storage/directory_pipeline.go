package storage

import (
	"context"
	"maps"
	"sync"
)

// Directory observations are cumulative and detached from the scanner's live
// stack. Retain only the newest pending observation and progress, so a slow
// publisher cannot stop traversal or build an unbounded queue of stale trees.
type directoryMailbox struct {
	mu                  sync.Mutex
	wake                chan struct{}
	directory, progress object
	progressAfter       bool
}

func (q *directoryMailbox) offer(v object) {
	q.mu.Lock()
	if v["directory_update"] != nil {
		q.directory = v
		q.progressAfter = false
	} else {
		if q.progress == nil {
			q.progress = maps.Clone(v)
		} else {
			maps.Copy(q.progress, v)
		}
		q.progressAfter = true
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *directoryMailbox) take() []object {
	q.mu.Lock()
	defer q.mu.Unlock()
	events := []object{q.progress, q.directory}
	if q.progressAfter {
		events[0], events[1] = events[1], events[0]
	}
	q.progress, q.directory = nil, nil
	return events
}

// The scanner is the producer; merge/progress/publication callbacks stay on the
// caller goroutine, in order. Completion joins the producer, and any callback
// error cancels it before returning. Final publication remains synchronous.
func inspectWithPublication(ctx context.Context, c Config, inspection DirectoryInspection, consume func(object) error) (*physicalScan, error) {
	if !inspection.StreamDirectory {
		return InspectDirectories(ctx, c, inspection, consume)
	}
	return directoryPipeline(ctx, func(ctx context.Context, progress func(object) error) (*physicalScan, error) {
		return InspectDirectories(ctx, c, inspection, progress)
	}, consume)
}

func directoryPipeline(ctx context.Context, scan func(context.Context, func(object) error) (*physicalScan, error), consume func(object) error) (*physicalScan, error) {
	ctx, cancel := context.WithCancel(ctx)
	q := &directoryMailbox{wake: make(chan struct{}, 1)}
	type outcome struct {
		result *physicalScan
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := scan(ctx, func(v object) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			q.offer(v)
			return nil
		})
		done <- outcome{result, err}
	}()
	joined, observed := false, false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	consumePending := func(final bool) error {
		for _, v := range q.take() {
			if v == nil {
				continue
			}
			if v["directory_update"] != nil {
				// Once traversal has finished, its final result supersedes
				// another pending checkpoint. Keep the first observation so
				// even small scans retain the existing live/cancel contract.
				if final && observed {
					continue
				}
				observed = true
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := consume(v); err != nil {
				return err
			}
		}
		return nil
	}
	finish := func(out outcome) (*physicalScan, error) {
		joined = true
		if out.err != nil {
			return nil, out.err
		}
		if err := consumePending(true); err != nil {
			return nil, err
		}
		return out.result, ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case out := <-done:
			return finish(out)
		case <-q.wake:
			// Prefer the final result over redundant queued observations.
			select {
			case out := <-done:
				return finish(out)
			default:
			}
			if err := consumePending(false); err != nil {
				return nil, err
			}
		}
	}
}
