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

	"project-alpha/internal/chatconfig"
)

func TestDeployedCLIUsesDefaultsWithoutConnectionFlags(t *testing.T) {
	token := strings.Repeat("a", 64)
	calls, logins := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/api/status/alice/login" {
			logins++
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to login")
			}
			json.NewEncoder(w).Encode(map[string]string{"session_token": token})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing token")
		}
		switch r.URL.Path {
		case api + "/me":
			io.WriteString(w, `{"id":"member-a","username":"alice"}`)
		case api + "/sessions":
			io.WriteString(w, `{"sessions":[]}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	exe := filepath.Join(t.TempDir(), "renamed-command")
	write := func(username string) {
		t.Helper()
		raw, _ := json.Marshal(chatconfig.Defaults{Server: server.URL, Username: username})
		if err := os.WriteFile(exe+".json", raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("alice")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--password-stdin", "--list"}, strings.NewReader("Member-password-123\n"), &out, &out, exe); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--list"}, strings.NewReader(""), &out, &out, exe); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatal("saved login not reused", logins)
	}
	before := calls
	write("bob")
	if err := run(context.Background(), []string{"--list"}, strings.NewReader(""), &out, &out, exe); err == nil {
		t.Fatal("changed identity reused login")
	}
	if calls != before {
		t.Fatal("old token sent after changing identity")
	}
}

func TestMalformedDeploymentDefaultsFailExplicitly(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "chat")
	for _, raw := range []string{`{}`, `{"server":"https://control.test","username":"alice","token":"secret"}`, `{"server":"https://control.test","username":"alice"}{}`, `{"server":"http://user:pass@host","username":"alice"}`} {
		if err := os.WriteFile(exe+".json", []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readDefaults(exe); err == nil {
			t.Fatal("invalid defaults accepted", raw)
		}
	}
}
