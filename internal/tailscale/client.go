package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
)

const apiBase = "https://api.tailscale.com/api/v2"
const maxResponseBytes = 8 << 20

// Device is a whitelist of management fields. Keys and physical endpoints are not exposed.
type Device struct {
	NodeID             string   `json:"nodeId"`
	Name               string   `json:"name"`
	Hostname           string   `json:"hostname"`
	Addresses          []string `json:"addresses"`
	OS                 string   `json:"os"`
	User               string   `json:"user"`
	ClientVersion      string   `json:"clientVersion"`
	Tags               []string `json:"tags"`
	ConnectedToControl *bool    `json:"connectedToControl"`
	LastSeen           string   `json:"lastSeen"`
	Authorized         bool     `json:"authorized"`
	IsExternal         bool     `json:"isExternal"`
}

func newClient() *http.Client {
	return &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (h *Handler) listDevices(ctx context.Context, s Settings, token string) ([]Device, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "GET", apiBase+"/tailnet/"+url.PathEscape(s.Tailnet)+"/devices", nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(r)
	if err != nil {
		// Never return transport errors or upstream bodies that could echo credentials.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, httpapi.NewError(504, "Tailscale 请求超时，请稍后重试")
		}
		return nil, httpapi.NewError(502, "无法连接 Tailscale API，请检查服务端网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case 401:
			return nil, httpapi.NewError(502, "Tailscale API Token 无效或已过期，请更换凭据")
		case 403:
			return nil, httpapi.NewError(502, "Tailscale 凭据无权查询此网络，请检查账号权限和 Tailnet")
		case 404:
			return nil, httpapi.NewError(502, "Tailscale 网络不存在，请检查 Tailnet ID 或名称")
		case 429:
			return nil, httpapi.NewError(503, "Tailscale 请求过于频繁，请稍后重试")
		default:
			return nil, httpapi.NewError(502, "Tailscale API 返回异常，请稍后重试")
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, httpapi.NewError(504, "Tailscale 请求超时，请稍后重试")
		}
		return nil, httpapi.NewError(502, "读取 Tailscale 响应失败，请重试")
	}
	var result struct {
		Devices []Device `json:"devices"`
	}
	if len(body) > maxResponseBytes || json.Unmarshal(body, &result) != nil || result.Devices == nil {
		return nil, httpapi.NewError(502, "Tailscale 节点响应格式无效或数据过大")
	}
	for i := range result.Devices {
		d := &result.Devices[i]
		if d.NodeID == "" {
			return nil, httpapi.NewError(502, "Tailscale 节点响应缺少 nodeId")
		}
		if d.Addresses == nil {
			d.Addresses = []string{}
		}
		if d.Tags == nil {
			d.Tags = []string{}
		}
	}
	sort.Slice(result.Devices, func(i, j int) bool {
		a, b := strings.ToLower(result.Devices[i].Hostname), strings.ToLower(result.Devices[j].Hostname)
		if a == b {
			return result.Devices[i].NodeID < result.Devices[j].NodeID
		}
		return a < b
	})
	return result.Devices, nil
}
