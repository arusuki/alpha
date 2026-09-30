package testutil

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"project-alpha/internal/httpapi"
	"testing"
)

type Client struct {
	T            *testing.T
	Handler      http.Handler
	Cookie, CSRF string
}

func (c *Client) Request(method, path string, value any, headers map[string]string) (int, map[string]any, *httptest.ResponseRecorder) {
	c.T.Helper()
	raw := []byte("{}")
	if value != nil {
		raw = []byte(httpapi.JSONText(value))
	}
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1")
	r.Header.Set("X-CSRF-Token", c.CSRF)
	if c.Cookie != "" {
		r.Header.Set("Cookie", c.Cookie)
	}
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	c.Handler.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w
}

func (c *Client) Expect(status int, method, path string, value any, headers map[string]string) map[string]any {
	c.T.Helper()
	actual, out, w := c.Request(method, path, value, headers)
	if actual != status {
		c.T.Fatalf("%s %s: want %d got %d: %s", method, path, status, actual, w.Body.String())
	}
	return out
}

func (c *Client) Login(setup bool, name, password string) {
	c.T.Helper()
	route := "/api/login"
	if setup {
		route = "/api/setup"
	}
	status, out, w := c.Request("POST", route, map[string]any{"username": name, "password": password}, nil)
	if status != 200 {
		c.T.Fatalf("login: %d %v", status, out)
	}
	c.CSRF = out["csrf"].(string)
	c.Cookie = w.Result().Cookies()[0].String()
}
