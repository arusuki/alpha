package chatcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordTokenConversationResumeAndLogout(t *testing.T) {
	token, session := strings.Repeat("a", 64), strings.Repeat("b", 32)
	logins, sends := 0, 0
	revoked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/status/alice/login" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["password"] != "Member-password-123" || r.Header.Get("Authorization") != "" {
				t.Error("bad login")
			}
			logins++
			json.NewEncoder(w).Encode(map[string]string{"session_token": token})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || revoked {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == api+"/me":
			json.NewEncoder(w).Encode(map[string]string{"id": "member", "username": "alice"})
		case r.URL.Path == api+"/logout":
			revoked = true
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "GET" && r.URL.Path == api+"/sessions":
			json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{{"id": session, "title": "hello"}}})
		case r.Method == "GET" && r.URL.Path == api+"/sessions/"+session:
			json.NewEncoder(w).Encode(map[string]any{"id": session, "messages": []any{}})
		case r.Method == "POST" && (r.URL.Path == api+"/sessions" || r.URL.Path == api+"/sessions/"+session+"/messages"):
			sends++
			json.NewEncoder(w).Encode(map[string]any{"id": session, "messages": []map[string]string{{"role": "assistant", "content": "answer"}}})
		default:
			t.Error(r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "login.json")
	run := func(input string, args ...string) string {
		t.Helper()
		var out, status bytes.Buffer
		err := Run(context.Background(), append([]string{"--config", path}, args...), strings.NewReader(input), &out, &status)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String()+status.String(), token) || strings.Contains(out.String()+status.String(), "Member-password-123") {
			t.Fatal("credential printed")
		}
		return out.String()
	}
	if got := run("Member-password-123\nhello\nfollow up\n/exit\n", "--server", server.URL, "--user", "alice", "--password-stdin"); got != "answer\nanswer\n" {
		t.Fatal(got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "Member-password-123") {
		t.Fatal("password saved")
	}
	if got := run("", "--list"); !strings.Contains(got, session) {
		t.Fatal(got)
	}
	run("", "--session", session, "--message", "next")
	if logins != 1 || sends != 3 {
		t.Fatal(logins, sends)
	}
	run("", "--logout")
	if !revoked {
		t.Fatal("logout did not revoke")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestClientNeverFollowsRedirectOrRetriesFailedMessage(t *testing.T) {
	calls, leaks := 0, 0
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks++ }))
	defer sink.Close()
	redirect := true
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if redirect {
			http.Redirect(w, r, sink.URL, 307)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer origin.Close()
	c := newClient(origin.URL)
	defer c.http.CloseIdleConnections()
	c.token = strings.Repeat("a", 64)
	if err := c.request(context.Background(), "POST", api+"/sessions", map[string]string{"message": "hello"}, nil); err == nil {
		t.Fatal("redirect accepted")
	}
	redirect = false
	if err := c.request(context.Background(), "POST", api+"/sessions", map[string]string{"message": "hello"}, nil); err == nil {
		t.Fatal("failure accepted")
	}
	if leaks != 0 || calls != 2 {
		t.Fatal(leaks, calls)
	}
}

func TestConfigBindingAndCancelledInput(t *testing.T) {
	for _, raw := range []string{"", "http://user:pass@host", "http://host/path", "http://host?token=secret", "file:///tmp"} {
		if _, err := serverURL(raw); err == nil {
			t.Fatal(raw)
		}
	}
	path := filepath.Join(t.TempDir(), "login.json")
	if err := saveState(path, loginState{"http://127.0.0.1", "alice", strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(path); err == nil {
		t.Fatal("insecure token file accepted")
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := Run(ctx, []string{"--config", filepath.Join(t.TempDir(), "none"), "--server", "http://127.0.0.1", "--user", "alice", "--password-stdin"}, input, &out, &out); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
}
