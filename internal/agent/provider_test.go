package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderSessionHeaderIsExplicit(t *testing.T) {
	for _, protocol := range []string{"responses", "completions"} {
		for _, provider := range []string{"openai", "opencode"} {
			t.Run(provider+"/"+protocol, func(t *testing.T) {
				var identities []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					identities = append(identities, r.Header.Get("x-opencode-session"))
					if protocol == "completions" {
						writeModelReply(w, object{"choices": []object{{"message": object{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
					} else {
						writeModelReply(w, object{"status": "completed", "output": []object{{"type": "message", "content": []object{{"type": "output_text", "text": "ok"}}}}})
					}
				}))
				defer server.Close()
				cfg := defaultConfig()
				cfg.Provider, cfg.Protocol, cfg.Endpoint = provider, protocol, server.URL
				for i, session := range []string{"session-a", "session-a", "session-b", ""} {
					_, err := (agentProvider{Config: cfg, SessionID: session}).complete(context.Background(), nil)
					if provider == "opencode" && session == "" {
						if err == nil || !strings.Contains(err.Error(), "会话 ID") || len(identities) != i {
							t.Fatal("missing session must fail before HTTP", err, identities)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					want := ""
					if provider == "opencode" {
						want = session
					}
					if identities[i] != want {
						t.Fatalf("header = %q, want %q", identities[i], want)
					}
				}
			})
		}
	}
}
