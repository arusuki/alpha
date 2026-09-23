package agent

import (
	"context"
	"database/sql"
	"sync"
	"time"
	"unicode/utf8"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const cleanupStreamLimitBytes = 64 * 1024 * 1024
const cleanupTraceTailBytes = 32 * 1024

type traceTail struct {
	text    string
	dropped int64
}

func traceTailText(value string) (string, int64) {
	if len(value) <= cleanupTraceTailBytes {
		return value, 0
	}
	start := len(value) - cleanupTraceTailBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:], int64(start)
}

func (t *traceTail) append(value string) {
	tail, dropped := traceTailText(t.text + value)
	t.text = tail
	t.dropped += dropped
}

// Trace uses event IDs for resuming. Cleanup requests replace each older public
// delta snapshot with a bounded current one; provider replay state stays private.
func (a *Manager) complete(ctx context.Context, id, groupID string, c Config, history []object, round int) (modelReply, error) {
	return a.completeWithTools(ctx, id, groupID, c, history, round, true)
}

func (a *Manager) completeWithTools(ctx context.Context, id, groupID string, c Config, history []object, round int, allowTools bool) (modelReply, error) {
	requestID := platform.RandomHex(12)
	started, flushed := time.Now(), time.Time{}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var recordErr error
	var pending []modelDelta
	var cleanupText, cleanupSummary, cleanupReasoning traceTail
	var cleanupChanged bool
	var cleanupFragments int
	var previousSnapshotID int64
	flush := func() error {
		if recordErr != nil {
			return recordErr
		}
		if len(pending) == 0 && !cleanupChanged {
			return nil
		}
		var err error
		if !allowTools {
			deltas := []modelDelta{}
			for _, row := range []struct {
				kind string
				tail traceTail
			}{{"text_snapshot", cleanupText}, {"summary_snapshot", cleanupSummary}, {"reasoning_snapshot", cleanupReasoning}} {
				if row.tail.text != "" || row.tail.dropped != 0 {
					deltas = append(deltas, modelDelta{Kind: row.kind, Text: row.tail.text, DroppedBytes: row.tail.dropped})
				}
			}
			payload := httpapi.JSONText(object{"group_id": groupID, "request_id": requestID, "deltas": deltas})
			var newSnapshotID int64
			err = a.db.Transaction(func(tx *sql.Tx) error {
				result, insertErr := tx.Exec("INSERT INTO agent_messages(session_id,role,content,created_at) VALUES(?,'model_delta',?,?)", id, payload, platform.Now())
				if insertErr != nil {
					return insertErr
				}
				newSnapshotID, insertErr = result.LastInsertId()
				if insertErr != nil {
					return insertErr
				}
				if previousSnapshotID != 0 {
					_, insertErr = tx.Exec("DELETE FROM agent_messages WHERE id=? AND session_id=? AND role='model_delta'", previousSnapshotID, id)
				}
				return insertErr
			})
			if err == nil {
				previousSnapshotID = newSnapshotID
				cleanupChanged = false
				cleanupFragments = 0
			}
		} else {
			err = a.message(id, "model_delta", httpapi.JSONText(object{"group_id": groupID, "request_id": requestID, "deltas": pending}), "")
		}
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
	providerSession := id
	if groupID != "" {
		providerSession += "-" + groupID
	}
	provider := agentProvider{Config: c, SessionID: providerSession,
		OnRequest: func(request object) error {
			request["request_id"], request["round"], request["group_id"] = requestID, round+1, groupID
			if !allowTools {
				delete(request, "context")
			}
			return a.message(id, "model_request", httpapi.JSONText(request), "")
		},
		OnDelta: func(delta modelDelta) error {
			mu.Lock()
			defer mu.Unlock()
			if recordErr != nil {
				return recordErr
			}
			if !allowTools {
				switch delta.Kind {
				case "text":
					cleanupText.append(delta.Text)
				case "summary":
					cleanupSummary.append(delta.Text)
				case "reasoning":
					cleanupReasoning.append(delta.Text)
				default:
					return nil
				}
				cleanupChanged = true
				cleanupFragments++
				if time.Since(flushed) >= 150*time.Millisecond || cleanupFragments >= 32 {
					return flush()
				}
				return nil
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
	if !allowTools {
		provider.StreamLimitBytes = cleanupStreamLimitBytes
	}
	reply, err := provider.complete(streamCtx, history, allowTools)
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
	responseText, responseSummary, responseReasoning := reply.Text, reply.Summary, reply.Reasoning
	var textDropped, summaryDropped, reasoningDropped int64
	if !allowTools {
		responseText, textDropped = traceTailText(responseText)
		responseSummary, summaryDropped = traceTailText(responseSummary)
		responseReasoning, reasoningDropped = traceTailText(responseReasoning)
	}
	if saveErr := a.message(id, "model_response", httpapi.JSONText(object{
		"group_id": groupID, "request_id": requestID, "status": status, "error": message, "duration_ms": time.Since(started).Milliseconds(),
		"text": responseText, "summary": responseSummary, "reasoning": responseReasoning, "usage": reply.Usage,
		"text_dropped_bytes": textDropped, "summary_dropped_bytes": summaryDropped, "reasoning_dropped_bytes": reasoningDropped,
	}), ""); saveErr != nil {
		return reply, saveErr
	}
	return reply, err
}
