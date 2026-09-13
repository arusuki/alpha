package storage

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"project-alpha/internal/httpapi"
)

// A short SSE connection fits the server's write timeout. EventSource resumes
// using Last-Event-ID, so reconnects and skipped revisions lose no observations.
func (s *Handler) streamSnapshot(w http.ResponseWriter, r *http.Request, id string) (int, any, error) {
	value := r.URL.Query().Get("revision")
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		value = last
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 {
		return 0, nil, httpapi.NewError(400, "请提供当前扫描记录版本")
	}
	changes, err := s.snapshotChanges(id, revision)
	if err != nil {
		return 0, nil, err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, nil, httpapi.NewError(500, "无法建立目录更新连接")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		if next := numberInt64(changes["revision"]); next > revision {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: changes\ndata: %s\n\n", next, httpapi.JSONText(changes)); err != nil {
				return 0, nil, nil
			}
			revision = next
		} else if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
			return 0, nil, nil
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return 0, nil, nil
		case <-deadline.C:
			return 0, nil, nil
		case <-ticker.C:
		}
		// Recheck revoked sessions and read only the tiny head until it changes.
		if _, err := s.DB.Session(httpapi.SessionToken(r)); err != nil {
			return 0, nil, nil
		}
		var head int64
		if err := s.DB.SQL.QueryRow("SELECT coalesce((SELECT revision FROM snapshot_records WHERE job_id=?),0)", id).Scan(&head); err != nil {
			return 0, nil, nil
		}
		if head > revision {
			changes, err = s.snapshotChanges(id, revision)
			if err != nil {
				return 0, nil, nil
			}
		}
	}
}
