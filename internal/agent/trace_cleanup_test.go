package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestCleanupTraceKeepsLatestWindowWhileReturningCompleteReply(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	var userID string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='administrator'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	id := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model) VALUES(?,?,'extract','running',1,1,'completions','test')", id, userID); err != nil {
		t.Fatal(err)
	}
	first, second := strings.Repeat("甲", 22000), strings.Repeat("乙", 22000)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, text := range []string{first, second} {
			fmt.Fprintf(w, "data: %s\n\n", httpapi.JSONText(object{"choices": []object{{"index": 0, "delta": object{"content": text}}}}))
			w.(http.Flusher).Flush()
			time.Sleep(180 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer mock.Close()
	c := defaultConfig()
	c.Protocol, c.Endpoint, c.Model = "completions", mock.URL, "test"
	reply, err := p.agent.completeWithTools(context.Background(), id, "", c, []object{{"role": "user", "content": "full context should not persist"}}, 0, false)
	if err != nil || reply.Text != first+second {
		t.Fatalf("complete extraction reply was truncated: length=%d err=%v", len(reply.Text), err)
	}
	var requestText, deltaText, responseText string
	if err := p.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='model_request'", id).Scan(&requestText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requestText, "full context should not persist") {
		t.Fatal("cleanup request persisted its full input context")
	}
	var count int
	if err := p.db.SQL.QueryRow("SELECT count(*),max(content) FROM agent_messages WHERE session_id=? AND role='model_delta'", id).Scan(&count, &deltaText); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one rolling trace snapshot, got %d", count)
	}
	var snapshot struct {
		Deltas []modelDelta `json:"deltas"`
	}
	if err := json.Unmarshal([]byte(deltaText), &snapshot); err != nil || len(snapshot.Deltas) != 1 || snapshot.Deltas[0].Kind != "text_snapshot" || len(snapshot.Deltas[0].Text) > cleanupTraceTailBytes || snapshot.Deltas[0].DroppedBytes == 0 || !strings.HasSuffix(reply.Text, snapshot.Deltas[0].Text) {
		t.Fatalf("trace snapshot was not bounded: count=%d err=%v", len(snapshot.Deltas), err)
	}
	if err := p.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='model_response'", id).Scan(&responseText); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Text             string `json:"text"`
		TextDroppedBytes int64  `json:"text_dropped_bytes"`
	}
	if err := json.Unmarshal([]byte(responseText), &response); err != nil || len(response.Text) > cleanupTraceTailBytes || response.TextDroppedBytes == 0 || !strings.HasSuffix(reply.Text, response.Text) {
		t.Fatalf("final visible response was not bounded: length=%d err=%v", len(response.Text), err)
	}
}
