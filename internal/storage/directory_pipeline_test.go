package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDirectoryPipelineOverlapsScanAndCoalescesUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	consuming, produced, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := &physicalScan{Visited: 1000}
	count := 0
	got, err := directoryPipeline(ctx, func(ctx context.Context, offer func(object) error) (*physicalScan, error) {
		if err := offer(object{"directory_update": &Node{}, "entries": 0}); err != nil {
			return nil, err
		}
		select {
		case <-consuming:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		for i := 1; i <= 1000; i++ {
			if err := offer(object{"directory_update": &Node{}, "entries": i}); err != nil {
				return nil, err
			}
		}
		close(produced)
		select {
		case <-finish:
			return result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, func(v object) error {
		count++
		if count == 1 {
			close(consuming)
			select {
			case <-produced:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("scanner blocked on publication: %w", ctx.Err())
			}
		}
		if count != 2 || v["entries"] != 1000 {
			return fmt.Errorf("queued stale observations instead of the newest: %v", v)
		}
		close(finish)
		return nil
	})
	if err != nil || got != result || count != 2 {
		t.Fatalf("pipeline did not overlap and coalesce: count=%d err=%v", count, err)
	}
}

func TestDirectoryPipelineErrorCancelsAndJoinsScanner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	failure := errors.New("publication failed")
	_, err := directoryPipeline(ctx, func(ctx context.Context, offer func(object) error) (*physicalScan, error) {
		defer close(stopped)
		if err := offer(object{"directory_update": &Node{}}); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(object) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("lost publication failure: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("returned while scanner was still running")
	}
}
