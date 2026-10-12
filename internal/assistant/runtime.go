package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const runtimeProtocol = 2
const agnoVersion = "3.1.2"
const mcpVersion = "2.1.0"
const openaiVersion = "3.29.0"
const maxRuntimeFrame = 16 << 20

type serviceClient struct{ socket string }
type runtimeEvent struct {
	Type          string `json:"type"`
	Code          string `json:"code"`
	Protocol      int    `json:"protocol"`
	Ready         bool   `json:"ready"`
	AgnoVersion   string `json:"agno_version"`
	MCPVersion    string `json:"mcp_version"`
	OpenAIVersion string `json:"openai_version"`
	ActiveRuns    int    `json:"active_runs"`
	Capacity      int    `json:"capacity"`
	ToolCall
	result
}
type Health struct {
	Status      string  `json:"status"`
	Detail      string  `json:"detail"`
	CheckedAt   float64 `json:"checked_at"`
	AgnoVersion string  `json:"agno_version,omitempty"`
	ActiveRuns  int     `json:"active_runs"`
	Capacity    int     `json:"capacity"`
}

func newServiceClient(defaultSocket string) *serviceClient {
	path := os.Getenv("ALPHA_AGNO_SOCKET")
	if path == "" {
		path = defaultSocket
	}
	return &serviceClient{socket: path}
}
func runtimeError(code string) error {
	switch code {
	case "busy":
		return httpapi.NewError(429, "Agno 容器服务繁忙，请稍后重试")
	case "dependencies", "protocol":
		return httpapi.NewError(503, "Agno 容器版本不匹配，请使用当前发布包重建服务")
	case "unavailable":
		return httpapi.NewError(503, "Agno 容器服务不可用，请检查容器及 socket 配置")
	case "mcp":
		return httpapi.NewError(502, "MCP 连接或工具调用失败，请检查服务地址、认证和工具名称；本轮未保存，远端操作可能已执行，请核实")
	case "state":
		return httpapi.NewError(409, "Agno 会话格式不匹配，已有数据保留；请使用新数据目录")
	default:
		return httpapi.NewError(502, "Agno 模型请求失败，请检查 Agent 设置、模型工具调用能力或网络；本轮未保存，未自动重试")
	}
}
func compatible(event runtimeEvent) bool {
	return event.Protocol == runtimeProtocol && event.MCPVersion == mcpVersion && event.AgnoVersion == agnoVersion && event.OpenAIVersion == openaiVersion && event.Capacity == 4 && event.ActiveRuns >= 0 && event.ActiveRuns <= event.Capacity
}
func writeFrame(conn net.Conn, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw)+1 > maxRuntimeFrame {
		return httpapi.NewError(413, "Agno 会话或工具结果超出传输限制，本轮未保存")
	}
	raw = append(raw, '\n')
	defer clear(raw)
	for len(raw) > 0 {
		n, err := conn.Write(raw)
		if err != nil {
			return err
		}
		raw = raw[n:]
	}
	return nil
}
func scanEvent(scanner *bufio.Scanner) (runtimeEvent, error) {
	var event runtimeEvent
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &event) != nil {
		return event, httpapi.NewError(502, "Agno 容器服务未返回有效结果，本轮未保存")
	}
	return event, nil
}
func (c *serviceClient) connect(ctx context.Context, operation string) (net.Conn, *bufio.Scanner, func(), error) {
	if !filepath.IsAbs(c.socket) {
		return nil, nil, nil, httpapi.NewError(503, "ALPHA_AGNO_SOCKET 必须为本机 socket 的绝对路径")
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, nil, nil, runtimeError("unavailable")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	closeConn := func() { stop(); conn.Close() }
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err = writeFrame(conn, map[string]any{"operation": operation, "protocol": runtimeProtocol}); err != nil {
		closeConn()
		return nil, nil, nil, runtimeError("unavailable")
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 65536), maxRuntimeFrame)
	return conn, scanner, closeConn, nil
}
func (c *serviceClient) health(ctx context.Context) Health {
	health := Health{Status: "unavailable", Detail: "Agno 容器服务不可用，请检查容器及 socket 配置", CheckedAt: platform.Now()}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, scanner, closeConn, err := c.connect(ctx, "health")
	if err != nil {
		return health
	}
	defer closeConn()
	event, err := scanEvent(scanner)
	if err != nil {
		return health
	}
	if event.Type != "health" || !compatible(event) {
		health.Status = "incompatible"
		health.Detail = "Agno 容器版本或协议不匹配，请重建当前版本服务"
		return health
	}
	if !event.Ready {
		health.Detail = "Agno 容器正在关闭"
		return health
	}
	health.Status = "ready"
	health.Detail = "Agno 服务就绪"
	health.AgnoVersion = event.AgnoVersion
	health.ActiveRuns = event.ActiveRuns
	health.Capacity = event.Capacity
	return health
}
func (h *Handler) Health(ctx context.Context) Health { return h.runtime.health(ctx) }

// The Go control talks only to a local socket owned by its service user. The
// container never receives a database mount, Docker socket or node credentials.
func (c *serviceClient) run(ctx context.Context, in input, tool func(ToolCall) (any, error)) (out result, err error) {
	defer func() {
		if ctx.Err() != nil {
			err = httpapi.NewError(408, "运维助理请求已取消或超时，本轮未保存")
		}
	}()
	conn, scanner, closeConn, err := c.connect(ctx, "run")
	if err != nil {
		return out, err
	}
	defer closeConn()
	greeting, err := scanEvent(scanner)
	if err != nil {
		return out, err
	}
	if greeting.Type == "error" {
		return out, runtimeError(greeting.Code)
	}
	if greeting.Type != "ready" || !compatible(greeting) || !greeting.Ready {
		return out, runtimeError("protocol")
	}
	deadline := time.Now().Add(15 * time.Minute)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	_ = conn.SetDeadline(deadline)
	if err = writeFrame(conn, in); err != nil {
		return out, err
	}
	for {
		event, e := scanEvent(scanner)
		if e != nil {
			return out, e
		}
		switch event.Type {
		case "tool":
			value, e := tool(event.ToolCall)
			reply := map[string]any{"value": value}
			if e != nil {
				reply = map[string]any{"error": "节点查询失败；该节点数据不可用，请稍后在工作台核实"}
			}
			if e = writeFrame(conn, reply); e != nil {
				return out, e
			}
		case "result":
			return event.result, nil
		case "error":
			return out, runtimeError(event.Code)
		default:
			return out, httpapi.NewError(502, "Agno 容器服务事件无效，本轮未保存")
		}
	}
}
