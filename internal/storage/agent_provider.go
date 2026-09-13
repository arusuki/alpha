package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
)

type agentToolCall struct{ ID, Name, Arguments string }
type modelReply struct {
	Text  string
	Calls []agentToolCall
	Items []object
}

type agentProvider struct {
	Config AgentConfig
}

// Keep endpoint and credentials inside this adapter; neither is exposed to tools.
func (p agentProvider) complete(ctx context.Context, history []object, allowTools bool) (modelReply, error) {
	var reply modelReply
	endpoint, err := p.Config.endpointURL()
	if err != nil {
		return reply, err
	}
	body := object{"model": p.Config.Model, "stream": false}
	if p.Config.Protocol == "completions" {
		body["messages"] = history
	} else {
		body["input"] = history
		body["store"] = false
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if allowTools {
		body["tools"] = agentToolDefinitions(p.Config.Protocol)
		body["tool_choice"] = "auto"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return reply, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return reply, fmt.Errorf("无法构造模型请求")
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return reply, fmt.Errorf("模型请求被取消或超时: %w", ctx.Err())
		}
		return reply, fmt.Errorf("无法连接模型 Endpoint，请检查地址、网络和 TLS 配置")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil {
		return reply, fmt.Errorf("读取模型响应失败")
	}
	if len(data) > 8*1024*1024 {
		return reply, fmt.Errorf("模型响应超过 8 MB 限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not persist provider error bodies: proxies can reflect credentials.
		return reply, fmt.Errorf("模型接口返回 HTTP %d，请检查接口类型、模型、API Key 或服务配额", resp.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message      object `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Output []object        `json:"output"`
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil {
		return reply, fmt.Errorf("模型接口返回了无效 JSON")
	}
	if len(result.Error) > 0 && string(result.Error) != "null" {
		return reply, fmt.Errorf("模型接口返回错误，请检查模型服务配置")
	}
	if p.Config.Protocol == "completions" {
		if len(result.Choices) == 0 {
			return reply, fmt.Errorf("Chat Completions 响应缺少 choices")
		}
		choice := result.Choices[0]
		if choice.FinishReason == "length" || choice.FinishReason == "content_filter" {
			return reply, fmt.Errorf("模型输出未完成（%s）", choice.FinishReason)
		}
		message := choice.Message
		if message == nil {
			return reply, fmt.Errorf("Chat Completions 响应缺少 message")
		}
		message["role"] = "assistant"
		reply.Text, _ = message["content"].(string)
		if refusal, _ := message["refusal"].(string); reply.Text == "" && refusal != "" {
			reply.Text = refusal
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				f, _ := call["function"].(map[string]any)
				if call["type"] != "function" {
					return reply, fmt.Errorf("模型请求了不支持的工具类型")
				}
				reply.Calls = append(reply.Calls, agentToolCall{httpapi.String(call["id"]), httpapi.String(f["name"]), httpapi.String(f["arguments"])})
			}
		}
		reply.Items = []object{message}
	} else {
		if result.Status != "" && result.Status != "completed" {
			return reply, fmt.Errorf("Responses 输出未完成（%s）", result.Status)
		}
		reply.Items = result.Output // Replay all items, including encrypted reasoning, before function outputs.
		for _, item := range result.Output {
			switch item["type"] {
			case "function_call":
				reply.Calls = append(reply.Calls, agentToolCall{httpapi.String(item["call_id"]), httpapi.String(item["name"]), httpapi.String(item["arguments"])})
			case "message":
				content, _ := item["content"].([]any)
				for _, raw := range content {
					part, _ := raw.(map[string]any)
					if part["type"] == "output_text" {
						reply.Text += httpapi.String(part["text"])
					}
					if part["type"] == "refusal" {
						reply.Text += httpapi.String(part["refusal"])
					}
				}
			}
		}
	}
	if len(reply.Calls) > 16 {
		return reply, fmt.Errorf("单轮工具调用超过 16 次限制")
	}
	ids := map[string]bool{}
	for _, c := range reply.Calls {
		if c.ID == "" || c.Name == "" || ids[c.ID] || len(c.Arguments) > 16384 {
			return reply, fmt.Errorf("模型工具调用格式无效")
		}
		ids[c.ID] = true
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Calls) == 0 {
		return reply, fmt.Errorf("模型未返回文字或工具调用")
	}
	return reply, nil
}

func toolOutput(protocol string, call agentToolCall, result any) object {
	if protocol == "completions" {
		return object{"role": "tool", "tool_call_id": call.ID, "content": httpapi.JSONText(result)}
	}
	return object{"type": "function_call_output", "call_id": call.ID, "output": httpapi.JSONText(result)}
}
