package bastion

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProxyPreservesStatusPathHostOriginAndToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "100.64.0.2:9765" || r.Header.Get("Origin") != "http://100.64.0.2:9765" || r.URL.RequestURI() != "/api/status/alice/containers?check=1" || r.Header.Get("Authorization") != "Bearer member-token" || r.Header.Get("X-CSRF-Token") != "csrf" {
			t.Errorf("lost request: %s %s %v", r.Host, r.URL, r.Header)
		}
		if r.Header.Get("X-Forwarded-Host") != "" {
			t.Error("trusted client forwarding header")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"node_id":"compute"}` {
			t.Error(string(body))
		}
		w.WriteHeader(202)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	handler, closeTransport, err := newShareProxy(installation{ListenHost: "100.64.0.2", StatusPort: 9765, ControlURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	r, _ := http.NewRequest("POST", proxy.URL+"/api/status/alice/containers?check=1", strings.NewReader(`{"node_id":"compute"}`))
	r.Host = "100.64.0.2:9765"
	r.Header.Set("Origin", "http://100.64.0.2:9765")
	r.Header.Set("Authorization", "Bearer member-token")
	r.Header.Set("X-CSRF-Token", "csrf")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	r, _ = http.NewRequest("GET", proxy.URL+"/status/alice", nil)
	r.Host = "evil.example"
	bad, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatal("untrusted Host accepted")
	}
}
func TestProxyRejectsVariableTargetsAndReportsUnavailableControl(t *testing.T) {
	for _, target := range []string{"", "file:///etc/passwd", "http://user:pass@control", "http://control/path", "http://control/?token=secret", "http://control?", "http://control/%2F", "http://control:0", "http://control:65536"} {
		if _, err := proxyTarget(target); err == nil {
			t.Fatal(target)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := upstream.URL
	upstream.Close()
	handler, closeTransport, err := newShareProxy(installation{ListenHost: "100.64.0.2", StatusPort: 9765, ControlURL: address})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	r := httptest.NewRequest("GET", "/status/alice", nil)
	r.Host = "100.64.0.2:9765"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
}

func TestShareProxyBlocksManagementBeforeUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer upstream.Close()
	handler, closeTransport, err := newShareProxy(installation{ListenHost: "100.64.0.2", StatusPort: 9765, ControlURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	for _, path := range []string{"/", "/nodes/" + strings.Repeat("a", 32) + "/", "/app.js", "/api/session", "/api/setup", "/api/login", "/api/cluster/overview", "/api/cluster/nodes", "/api/members", "/api/status/alice/../../cluster/overview", "/status/alice/../", "/api/status/alice/gpu/extra"} {
		for _, method := range []string{"GET", "POST"} {
			r := httptest.NewRequest(method, path, nil)
			r.Host = "100.64.0.2:9765"
			r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: "admin-cookie"})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	if calls != 0 {
		t.Fatal("blocked request reached control", calls)
	}
	for _, path := range []string{"/status/alice", "/status/alice/", "/status.js", "/status.css", "/disk-capacity.js", "/disk-capacity.css", "/gpu.js", "/gpu.css", "/member-disk.js", "/usage.js", "/clipboard.js", "/api/status/alice", "/api/status/alice/gpu", "/api/status/alice/disk"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "100.64.0.2:9765"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
