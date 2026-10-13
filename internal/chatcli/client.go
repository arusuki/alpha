// Package chatcli implements the independent member conversation client.
package chatcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)
var sessionPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var tokenPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type client struct {
	server, token string
	http          *http.Client
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.message) }

func serverURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return "", fmt.Errorf("--server 需为总控或分享入口的 HTTP(S) 地址，不含账号、路径或查询参数")
	}
	u.Path = ""
	return u.String(), nil
}

func newClient(server string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Credentials must not inherit an ambient HTTP proxy.
	return &client{server: server, http: &http.Client{Transport: transport, Timeout: 16 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) request(ctx context.Context, method, path string, body, result any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, input)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求未完成，请核对历史会话后重试: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("服务器响应超过大小限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(resp.StatusCode)
		}
		return &apiError{resp.StatusCode, safeText(failure.Error)}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("服务器响应格式无效: %w", err)
	}
	return nil
}

func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, s)
}
