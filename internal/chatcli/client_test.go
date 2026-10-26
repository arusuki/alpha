package chatcli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryRequestDeadline(t *testing.T) {
	c := newClient("http://control.invalid")
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 30*time.Second {
			t.Fatalf("ordinary request deadline: %v, %v", remaining, ok)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err := c.request(context.Background(), "POST", "/api/status/alice/login", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRequestTimeoutCoversHeadersAndBody(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[flushHeaders], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if flushHeaders {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			c := newClient(server.URL)
			defer c.http.CloseIdleConnections()
			err := c.requestWithTimeout(context.Background(), "GET", "/", nil, nil, 50*time.Millisecond)
			if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "历史会话") {
				t.Fatalf("unexpected timeout error: %v", err)
			}
		})
	}
}
