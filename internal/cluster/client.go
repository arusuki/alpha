package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (h *Control) request(ctx context.Context, node Node, method, path string, body any, user platform.User) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		defer clear(raw)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, node.URL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	authenticate(req, node, user)
	response, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("节点连接中断，请检查地址、网络与服务状态")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		status := response.StatusCode
		if status == 401 || status >= 300 && status < 400 {
			status = 502
		}
		var value struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&value)
		if value.Error == "" {
			value.Error = "节点请求失败"
		}
		return nil, httpapi.NewError(status, value.Error)
	}
	return response, nil
}

func (h *Control) call(ctx context.Context, node Node, method, path string, body any, user platform.User, out any) error {
	response, err := h.request(ctx, node, method, path, body, user)
	if err != nil {
		var apiErr *httpapi.Error
		if errors.As(err, &apiErr) {
			return err
		}
		return httpapi.NewError(502, err.Error())
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024+1))
	invalid := httpapi.NewError(502, "节点返回的数据格式无效或超出限制")
	if err != nil || len(raw) > 16*1024*1024 {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	return nil
}
