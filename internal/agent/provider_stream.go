package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"project-alpha/internal/httpapi"
)

// Optional counts stay unknown when the service does not report them.
type modelUsage struct {
	Input     int64  `json:"input_tokens"`
	Output    int64  `json:"output_tokens"`
	Total     int64  `json:"total_tokens"`
	Cached    *int64 `json:"cached_tokens"`
	Reasoning *int64 `json:"reasoning_tokens"`
}

func parseModelUsage(raw object, protocol string) *modelUsage {
	input, output, inputDetails, outputDetails := "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details"
	if protocol == "completions" {
		input, output, inputDetails, outputDetails = "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details"
	}
	count := func(v any) *int64 {
		n, ok := v.(float64)
		if !ok || n < 0 || n > 1e12 || n != float64(int64(n)) {
			return nil
		}
		i := int64(n)
		return &i
	}
	in, out := count(raw[input]), count(raw[output])
	if in == nil || out == nil {
		return nil
	}
	u := &modelUsage{Input: *in, Output: *out, Total: *in + *out}
	if details, ok := raw[inputDetails].(map[string]any); ok {
		u.Cached = count(details["cached_tokens"])
	}
	if details, ok := raw[outputDetails].(map[string]any); ok {
		u.Reasoning = count(details["reasoning_tokens"])
	}
	return u
}

type modelDelta struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Index     int    `json:"index,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Only explicit public fields enter the durable context viewer. Opaque reasoning
// is replayed in memory for the API, but never copied into messages or events.
func visibleContext(history []object) []object {
	visible := make([]object, 0, len(history))
	for _, item := range history {
		v := object{}
		if item["type"] == "reasoning" {
			v = object{"type": "reasoning", "note": "内部推理状态已在内存中传递，不保存或展示"}
		} else {
			for _, key := range []string{"role", "type", "content", "refusal", "tool_calls", "tool_call_id", "call_id", "name", "arguments", "output"} {
				if value, ok := item[key]; ok {
					v[key] = value
				}
			}
		}
		visible = append(visible, v)
	}
	return visible
}

func (p agentProvider) readStream(reader io.Reader) (modelReply, error) {
	const limit = 8 * 1024 * 1024
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), limit)
	var data []string
	message := object{"role": "assistant", "content": ""}
	calls := map[int]object{}
	finish := ""
	var usage object
	var reply modelReply
	emit := func(d modelDelta) error {
		if p.OnDelta != nil {
			return p.OnDelta(d)
		}
		return nil
	}
	consume := func(raw string) (bool, error) {
		if raw == "[DONE]" {
			if p.Config.Protocol != "completions" || finish == "" {
				return false, fmt.Errorf("模型流提前结束，未收到完整响应")
			}
			indices := make([]int, 0, len(calls))
			for i := range calls {
				indices = append(indices, i)
			}
			sort.Ints(indices)
			all := []object{}
			for _, i := range indices {
				all = append(all, calls[i])
			}
			if len(all) > 0 {
				message["tool_calls"] = all
			}
			var err error
			reply, err = parseModelReply([]byte(httpapi.JSONText(object{"choices": []object{{"message": message, "finish_reason": finish}}, "usage": usage})), "completions")
			return true, err
		}
		var event object
		if json.Unmarshal([]byte(raw), &event) != nil {
			return false, fmt.Errorf("模型流包含无效 JSON")
		}
		if event["error"] != nil || event["type"] == "error" {
			return false, fmt.Errorf("模型流返回错误，请检查模型服务配置")
		}
		if p.Config.Protocol == "completions" {
			if v, ok := event["usage"].(map[string]any); ok {
				usage = v
				reply.Usage = parseModelUsage(v, "completions")
			}
			choices, _ := event["choices"].([]any)
			for _, rawChoice := range choices {
				choice, _ := rawChoice.(map[string]any)
				if choice["index"] != float64(0) {
					continue
				}
				if f := httpapi.String(choice["finish_reason"]); f != "" {
					finish = f
				}
				delta, _ := choice["delta"].(map[string]any)
				text := httpapi.String(delta["content"]) + httpapi.String(delta["refusal"])
				if text != "" {
					message["content"] = httpapi.String(message["content"]) + text
					if err := emit(modelDelta{Kind: "text", Text: text}); err != nil {
						return false, err
					}
				}
				parts, _ := delta["tool_calls"].([]any)
				for _, rawPart := range parts {
					part, _ := rawPart.(map[string]any)
					n, ok := part["index"].(float64)
					if !ok || n < 0 || n >= 16 || n != float64(int(n)) {
						return false, fmt.Errorf("模型工具调用序号无效或超过 16 次限制")
					}
					i := int(n)
					if calls[i] == nil {
						calls[i] = object{"id": "", "type": "function", "function": object{"name": "", "arguments": ""}}
					}
					call := calls[i]
					f := call["function"].(object)
					fragment, _ := part["function"].(map[string]any)
					if typ := httpapi.String(part["type"]); typ != "" && typ != "function" {
						return false, fmt.Errorf("模型请求了不支持的工具类型")
					}
					call["id"] = httpapi.String(call["id"]) + httpapi.String(part["id"])
					f["name"] = httpapi.String(f["name"]) + httpapi.String(fragment["name"])
					f["arguments"] = httpapi.String(f["arguments"]) + httpapi.String(fragment["arguments"])
					if len(httpapi.String(f["arguments"])) > 16384 {
						return false, fmt.Errorf("模型工具参数超过限制")
					}
					if err := emit(modelDelta{Kind: "tool", Index: i, Name: httpapi.String(f["name"]), Arguments: httpapi.String(f["arguments"])}); err != nil {
						return false, err
					}
				}
			}
			return false, nil
		}
		switch event["type"] {
		case "response.output_text.delta", "response.refusal.delta":
			return false, emit(modelDelta{Kind: "text", Text: httpapi.String(event["delta"])})
		case "response.reasoning_summary_text.delta":
			return false, emit(modelDelta{Kind: "summary", Text: httpapi.String(event["delta"])})
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				n, _ := event["output_index"].(float64)
				if n < 0 || n > 1024 || n != float64(int(n)) || len(calls) >= 16 {
					return false, fmt.Errorf("模型工具调用格式无效或超过 16 次限制")
				}
				calls[int(n)] = item
				return false, emit(modelDelta{Kind: "tool", Index: int(n), Name: httpapi.String(item["name"]), Arguments: httpapi.String(item["arguments"])})
			}
		case "response.function_call_arguments.delta":
			n, _ := event["output_index"].(float64)
			item := calls[int(n)]
			if item == nil {
				return false, fmt.Errorf("模型工具参数缺少对应调用")
			}
			item["arguments"] = httpapi.String(item["arguments"]) + httpapi.String(event["delta"])
			if len(httpapi.String(item["arguments"])) > 16384 {
				return false, fmt.Errorf("模型工具参数超过限制")
			}
			return false, emit(modelDelta{Kind: "tool", Index: int(n), Name: httpapi.String(item["name"]), Arguments: httpapi.String(item["arguments"])})
		case "response.completed", "response.incomplete", "response.failed":
			result, ok := event["response"].(map[string]any)
			if !ok {
				return false, fmt.Errorf("模型流缺少完整响应")
			}
			var err error
			reply, err = parseModelReply([]byte(httpapi.JSONText(result)), "responses")
			if event["type"] != "response.completed" && err == nil {
				err = fmt.Errorf("模型输出未完成")
			}
			return true, err
		}
		return false, nil
	}
	for scanner.Scan() {
		if limited.N <= 0 {
			return reply, fmt.Errorf("模型响应超过 8 MB 限制")
		}
		line := scanner.Text()
		if line == "" {
			if len(data) == 0 {
				continue
			}
			done, err := consume(strings.Join(data, "\n"))
			data = nil
			if err != nil || done {
				return reply, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if limited.N <= 0 {
		return reply, fmt.Errorf("模型响应超过 8 MB 限制")
	}
	if scanner.Err() != nil {
		return reply, fmt.Errorf("读取模型流失败，连接中断或请求超时")
	}
	return reply, fmt.Errorf("模型流提前结束，未收到完整响应")
}
