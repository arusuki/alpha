package agent

import (
	"context"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Trace records append-only events, so reconnecting readers can resume by ID.
// Only the provider's public deltas enter this log; its replay state stays private.
func (a *Manager) complete(ctx context.Context, id string, c Config, history []object, round int) (modelReply, error) {
	requestID := platform.RandomHex(12)
	started, flushed := time.Now(), time.Time{}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var recordErr error
	var pending []modelDelta
	flush := func() error {
		if recordErr != nil {
			return recordErr
		}
		if len(pending) == 0 {
			return nil
		}
		err := a.message(id, "model_delta", httpapi.JSONText(object{"request_id": requestID, "deltas": pending}), "")
		if err == nil {
			pending = nil
			flushed = time.Now()
		}
		if err != nil {
			recordErr = err
			cancel()
		}
		return err
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mu.Lock()
				_ = flush()
				mu.Unlock()
			}
		}
	}()
	provider := agentProvider{Config: c, SessionID: id,
		OnRequest: func(request object) error {
			request["request_id"], request["round"] = requestID, round+1
			return a.message(id, "model_request", httpapi.JSONText(request), "")
		},
		OnDelta: func(delta modelDelta) error {
			mu.Lock()
			defer mu.Unlock()
			if recordErr != nil {
				return recordErr
			}
			// Merge adjacent text and replace partial arguments within each batch.
			if len(pending) > 0 {
				last := &pending[len(pending)-1]
				if last.Kind == delta.Kind && delta.Kind != "tool" {
					last.Text += delta.Text
				} else if last.Kind == "tool" && delta.Kind == "tool" && last.Index == delta.Index {
					*last = delta
				} else {
					pending = append(pending, delta)
				}
			} else {
				pending = append(pending, delta)
			}
			if time.Since(flushed) >= 150*time.Millisecond || len(pending) >= 32 {
				return flush()
			}
			return nil
		},
	}
	reply, err := provider.complete(streamCtx, history, true)
	close(stop)
	<-stopped
	if flushErr := flush(); flushErr != nil {
		return reply, flushErr
	}
	status, message := "completed", ""
	if err != nil {
		status, message = "failed", err.Error()
	}
	if ctx.Err() != nil {
		status, message = "cancelled", "模型请求已停止；保留已收到的内容"
	}
	if saveErr := a.message(id, "model_response", httpapi.JSONText(object{
		"request_id": requestID, "status": status, "error": message, "duration_ms": time.Since(started).Milliseconds(),
		"text": reply.Text, "summary": reply.Summary, "usage": reply.Usage,
	}), ""); saveErr != nil {
		return reply, saveErr
	}
	return reply, err
}
