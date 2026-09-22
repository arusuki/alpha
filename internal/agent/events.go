package agent

import (
	"fmt"
	"net/http"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (a *Manager) sessionUpdate(id, userID string, after int64) (object, error) {
	session, err := a.session(id, userID)
	if err != nil {
		return nil, err
	}
	messages, err := platform.Rows(a.db.SQL, "SELECT id,role,content,tool_name,created_at FROM agent_messages WHERE session_id=? AND id>? ORDER BY id LIMIT 201", id, after)
	if err != nil {
		return nil, err
	}
	hasMore := len(messages) > 200
	if hasMore {
		messages = messages[:200]
	}
	// Context snapshots can be large. Bound a page without truncating an event.
	bytes := 0
	for i, message := range messages {
		bytes += len(httpapi.String(message["content"]))
		if i > 0 && bytes > 1024*1024 {
			messages = messages[:i]
			hasMore = true
			break
		}
	}
	if len(messages) > 0 {
		after = messages[len(messages)-1]["id"].(int64)
	}
	var job any
	if jobID := httpapi.String(session["active_job_id"]); jobID != "" {
		job, err = a.records.Job(jobID)
		if err != nil {
			return nil, err
		}
	}
	return object{"session": session, "messages": messages, "next_after": after, "has_more": hasMore, "active_job": job}, nil
}

func (s *Handler) streamSession(w http.ResponseWriter, r *http.Request, id, userID string, after int64) (int, any, error) {
	update, err := s.Manager.sessionUpdate(id, userID, after)
	if err != nil {
		return 0, nil, err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, nil, httpapi.NewError(500, "无法建立分析进度连接")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		after = update["next_after"].(int64)
		if _, err := fmt.Fprintf(w, "id: %d\nevent: session\ndata: %s\n\n", after, httpapi.JSONText(update)); err != nil {
			return 0, nil, nil
		}
		flusher.Flush()
		status := update["session"].(object)["status"]
		busy := status == "queued" || status == "running" || status == "scanning" || status == "cancelling"
		if update["has_more"] != true {
			if !busy {
				return 0, nil, nil
			}
			select {
			case <-r.Context().Done():
				return 0, nil, nil
			case <-deadline.C:
				return 0, nil, nil
			case <-ticker.C:
			}
		}
		// A long connection must not retain access after logout or role revocation.
		auth, err := s.DB.Session(httpapi.SessionToken(r))
		if err != nil || auth.User.ID != userID || auth.User.Role != "admin" {
			return 0, nil, nil
		}
		update, err = s.Manager.sessionUpdate(id, userID, after)
		if err != nil {
			return 0, nil, nil
		}
	}
}
