package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func TestAnalysisStreamCancellationReplayAndAuthorization(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"private-raw-reasoning\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"公开进度：\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"检查目录\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer func() { p.agent.Close(); mock.Close() }()
	configureTestAgent(t, p, "responses", mock.URL)
	start := p.expect(202, "POST", "/api/agent/sessions", object{"message": "分析整个磁盘"}, nil)
	id := httpapi.String(start["id"])
	server := httptest.NewServer(p.s)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/agent/sessions/"+id+"/events?after=0", nil)
	req.Header.Set("Cookie", p.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("not SSE")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	var cursor int64
	var text string
	requestSeen := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var update struct {
			Messages []struct {
				ID      int64  `json:"id"`
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Next int64 `json:"next_after"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &update); err != nil {
			t.Fatal(err)
		}
		cursor = update.Next
		for _, m := range update.Messages {
			if strings.Contains(m.Content, "private-raw-reasoning") || strings.Contains(m.Content, "test-secret-key") {
				t.Fatal("private fields persisted")
			}
			if m.Role == "model_request" {
				requestSeen = strings.Contains(m.Content, "分析整个磁盘") && strings.Contains(m.Content, "全盘观察")
			}
			if m.Role == "model_delta" {
				var d struct {
					Deltas []modelDelta `json:"deltas"`
				}
				_ = json.Unmarshal([]byte(m.Content), &d)
				for _, delta := range d.Deltas {
					text += delta.Text
				}
			}
		}
		if strings.Contains(text, "检查目录") {
			break
		}
	}
	if !requestSeen || text != "公开进度：检查目录" {
		t.Fatalf("request or paused stream missing: %v %q %v", requestSeen, text, scanner.Err())
	}
	resp.Body.Close()
	p.expect(200, "POST", "/api/agent/sessions/"+id+"/cancel", object{}, nil)
	finished := waitAgentSession(t, p, id)
	if finished["session"].(object)["status"] != "cancelled" {
		t.Fatal(finished)
	}
	roles := map[string]int{}
	for _, raw := range finished["messages"].([]any) {
		m := raw.(object)
		roles[httpapi.String(m["role"])]++
		if m["role"] == "model_response" {
			var response object
			_ = json.Unmarshal([]byte(httpapi.String(m["content"])), &response)
			if response["status"] != "cancelled" || response["usage"] != nil {
				t.Fatal(response)
			}
		}
	}
	if roles["assistant"] != 0 || roles["model_response"] != 1 {
		t.Fatalf("partial output treated as final: %v", roles)
	}
	code, _, replay := p.request("GET", "/api/agent/sessions/"+id+"/events?after=0", nil, map[string]string{"Last-Event-ID": fmt.Sprint(cursor)})
	if code != 200 || !strings.Contains(replay.Body.String(), "model_response") || strings.Contains(replay.Body.String(), "model_request") {
		t.Fatalf("bad resume: %s", replay.Body.String())
	}
	p.expect(400, "GET", "/api/agent/sessions/"+id+"/events?after=-1", nil, nil)
	p.expect(201, "POST", "/api/users", object{"username": "anotheradmin", "password": "Second-admin-pass-123", "role": "admin"}, nil)
	p.login(false, "anotheradmin", "Second-admin-pass-123")
	p.expect(404, "GET", "/api/agent/sessions/"+id+"/events", nil, nil)
}

func TestResponsesToolFragmentsAndIncompleteUsage(t *testing.T) {
	p := agentProvider{Config: Config{Protocol: "responses"}}
	var deltas []modelDelta
	p.OnDelta = func(d modelDelta) error { deltas = append(deltas, d); return nil }
	stream := `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-a","name":"get_directory","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"/data\"}"}

data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call-a","name":"get_directory","arguments":"{\"path\":\"/data\"}"}]}}

`
	reply, err := p.readStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Calls) != 1 || len(deltas) != 3 || deltas[2].Arguments != `{"path":"/data"}` {
		t.Fatalf("bad fragments: %+v %+v", reply, deltas)
	}
	_, err = p.readStream(strings.NewReader(`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}` + "\n\n"))
	if err == nil {
		t.Fatal("accepted orphan arguments")
	}
	reply, err = p.readStream(strings.NewReader(`data: {"type":"response.incomplete","response":{"status":"incomplete","output":[],"usage":{"input_tokens":50,"output_tokens":10}}}` + "\n\n"))
	if err == nil || reply.Usage == nil || reply.Usage.Total != 60 {
		t.Fatalf("lost incomplete usage: %+v %v", reply, err)
	}
}
