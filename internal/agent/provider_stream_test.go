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
)

// Existing tool-loop fixtures now use the same streaming wire format as models.
func writeModelReply(w http.ResponseWriter, result object) {
	w.Header().Set("Content-Type", "text/event-stream")
	if choices, ok := result["choices"].([]object); ok {
		for i, choice := range choices {
			message := choice["message"].(object)
			if calls, ok := message["tool_calls"].([]object); ok {
				for j, call := range calls {
					call["index"] = j
				}
			}
			finish := choice["finish_reason"]
			if finish == nil {
				finish = "stop"
			}
			fmt.Fprintf(w, "data: %s\n\n", httpapi.JSONText(object{"choices": []object{{"index": i, "delta": message, "finish_reason": finish}}, "usage": result["usage"]}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	} else {
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", httpapi.JSONText(object{"type": "response.completed", "response": result}))
	}
	w.(http.Flusher).Flush()
}

func TestProviderStreamsBeforeCompletion(t *testing.T) {
	for _, protocol := range []string{"responses", "completions"} {
		t.Run(protocol, func(t *testing.T) {
			release := make(chan struct{})
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["stream"] != true {
					t.Error("stream disabled")
				}
				if protocol == "completions" && body["stream_options"].(object)["include_usage"] != true {
					t.Error("usage not requested")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "completions" {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先检查目录\",\"content\":\"正在核对\"}}]}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"先检查目录\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"正在核对\"}\n\n")
				}
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if protocol == "completions" {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"磁盘\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":30,\"total_tokens\":150,\"prompt_tokens_details\":{\"cached_tokens\":40},\"completion_tokens_details\":{\"reasoning_tokens\":10}}}\n\ndata: [DONE]\n\n")
				} else {
					writeModelReply(w, object{"status": "completed", "output": []object{{"type": "reasoning", "encrypted_content": "opaque-private", "summary": []object{{"type": "summary_text", "text": "先检查目录"}}}, {"type": "message", "role": "assistant", "content": []object{{"type": "output_text", "text": "正在核对磁盘"}}}}, "usage": object{"input_tokens": 120, "output_tokens": 30, "input_tokens_details": object{"cached_tokens": 40}, "output_tokens_details": object{"reasoning_tokens": 10}}})
				}
			}))
			defer mock.Close()
			config := defaultConfig()
			config.Protocol = protocol
			config.Endpoint = mock.URL
			deltas := make(chan modelDelta, 10)
			done := make(chan modelReply, 1)
			failures := make(chan error, 1)
			go func() {
				reply, err := (agentProvider{Config: config, OnDelta: func(d modelDelta) error { deltas <- d; return nil }}).complete(context.Background(), nil, true)
				done <- reply
				failures <- err
			}()
			select {
			case d := <-deltas:
				kind := "summary"
				if protocol == "completions" {
					kind = "reasoning"
				}
				if d.Kind != kind || d.Text != "先检查目录" {
					t.Errorf("missing incremental thinking: %+v", d)
				}
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("no incremental output")
			}
			select {
			case <-done:
				t.Error("completed before release")
			default:
			}
			close(release)
			reply := <-done
			if err := <-failures; err != nil {
				t.Fatal(err)
			}
			if reply.Text != "正在核对磁盘" || reply.Usage == nil || reply.Usage.Total != 150 || *reply.Usage.Cached != 40 || *reply.Usage.Reasoning != 10 {
				t.Fatalf("bad reply: %+v", reply)
			}
			if protocol == "responses" {
				if strings.TrimSpace(reply.Summary) != "先检查目录" || reply.Reasoning != "" {
					t.Fatalf("wrong Responses thinking: %+v", reply)
				}
				if !strings.Contains(httpapi.JSONText(reply.Items), "opaque-private") {
					t.Fatal("lost in-memory reasoning replay")
				}
				if strings.Contains(httpapi.JSONText(visibleContext(reply.Items)), "opaque-private") {
					t.Fatal("private reasoning entered context viewer")
				}
			} else if reply.Reasoning != "先检查目录" || reply.Summary != "" || reply.Items[0]["reasoning_content"] != reply.Reasoning {
				t.Fatalf("lost Completions reasoning or replay: %+v", reply)
			}
		})
	}
}

func TestCleanupStreamAcceptsDuplicatedResponsesCompletion(t *testing.T) {
	output := strings.Repeat("x", 5*1024*1024)
	stream := "data: " + httpapi.JSONText(object{"type": "response.output_text.delta", "delta": output}) + "\n\n" +
		"data: " + httpapi.JSONText(object{"type": "response.completed", "response": object{"status": "completed", "output": []object{{"type": "message", "content": []object{{"type": "output_text", "text": output}}}}}}) + "\n\n"
	if _, err := (agentProvider{Config: Config{Protocol: "responses"}}).readStream(strings.NewReader(stream)); err == nil {
		t.Fatal("ordinary model stream should retain its 8 MB limit")
	}
	reply, err := (agentProvider{Config: Config{Protocol: "responses"}, StreamLimitBytes: cleanupStreamLimitBytes}).readStream(strings.NewReader(stream))
	if err != nil || reply.Text != output {
		t.Fatalf("cleanup stream lost complete output: length=%d err=%v", len(reply.Text), err)
	}
}

func TestCompletionReasoningFragments(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			var stream strings.Builder
			for _, text := range []string{"先检查目录。\n", "再比较占用。"} {
				fmt.Fprintf(&stream, "data: %s\n\n", httpapi.JSONText(object{"choices": []object{{"index": 0, "delta": object{field: text}}}}))
			}
			prefix := stream.String()
			stream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"检查完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			var thinking string
			provider := agentProvider{Config: Config{Protocol: "completions"}, OnDelta: func(d modelDelta) error {
				if d.Kind == "reasoning" {
					thinking += d.Text
				}
				return nil
			}}
			reply, err := provider.readStream(strings.NewReader(stream.String()))
			want := "先检查目录。\n再比较占用。"
			if err != nil || thinking != want || reply.Reasoning != want || reply.Items[0][field] != want || reply.Text != "检查完成" {
				t.Fatalf("lost reasoning fragments: %+v %q %v", reply, thinking, err)
			}
			thinking = ""
			if _, err := provider.readStream(strings.NewReader(prefix)); err == nil || thinking != want {
				t.Fatalf("interrupted thinking lost or treated as complete: %q %v", thinking, err)
			}
		})
	}
}

func TestStreamToolArgumentFragmentsAndFailures(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"get_directory\",\"arguments\":\"{\\\"pa\"}},{\"index\":1,\"id\":\"b\",\"type\":\"function\",\"function\":{\"name\":\"get_overview\",\"arguments\":\"{}\"}}]}}]}\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\"/数据\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	p := agentProvider{Config: Config{Protocol: "completions"}}
	r, err := p.readStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 2 || r.Calls[0].Arguments != `{"path":"/数据"}` || r.Calls[1].ID != "b" || r.Usage != nil {
		t.Fatalf("bad calls: %+v", r)
	}
	for _, bad := range []string{
		strings.ReplaceAll(stream, "data: [DONE]\n\n", ""),
		strings.ReplaceAll(stream, `"finish_reason":"tool_calls"`, `"finish_reason":"length"`),
		strings.ReplaceAll(stream, `"id":"b"`, `"id":"a"`),
		"data: [DONE]\n\n", "data: {invalid}\n\n", "data: {\"error\":{\"message\":\"secret-key\"}}\n\n",
	} {
		if _, err := p.readStream(strings.NewReader(bad)); err == nil || strings.Contains(err.Error(), "secret-key") {
			t.Fatalf("accepted malformed stream or leaked error: %v", err)
		}
	}
}
