package assistant

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func fakeRuntime(t *testing.T, reply runtimeEvent) *serviceClient {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			var hello map[string]any
			_ = json.NewDecoder(conn).Decode(&hello)
			_ = json.NewEncoder(conn).Encode(reply)
			conn.Close()
		}
	}()
	return &serviceClient{socket: path}
}
func TestHealthChecksProtocolAndVersions(t *testing.T) {
	good := runtimeEvent{Type: "health", Protocol: runtimeProtocol, Ready: true, AgnoVersion: agnoVersion, OpenAIVersion: openaiVersion, MCPVersion: mcpVersion, Capacity: 4, ActiveRuns: 1}
	for _, tc := range []struct {
		name   string
		change func(*runtimeEvent)
		status string
	}{
		{"healthy", func(*runtimeEvent) {}, "ready"},
		{"protocol", func(e *runtimeEvent) { e.Protocol++ }, "incompatible"},
		{"dependency", func(e *runtimeEvent) { e.AgnoVersion = "unknown" }, "incompatible"},
		{"capacity", func(e *runtimeEvent) { e.ActiveRuns = 5 }, "incompatible"},
		{"stopping", func(e *runtimeEvent) { e.Ready = false }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := good
			tc.change(&reply)
			client := fakeRuntime(t, reply)
			health := client.health(context.Background())
			if health.Status != tc.status {
				t.Fatal(health)
			}
		})
	}
}
func TestRunRejectsHandshakeBeforeSendingCredentials(t *testing.T) {
	for _, code := range []string{"busy", "protocol", "unavailable"} {
		t.Run(code, func(t *testing.T) {
			client := fakeRuntime(t, runtimeEvent{Type: "error", Code: code})
			_, err := client.run(context.Background(), input{APIKey: "not-sent"}, nil)
			status := 503
			if code == "busy" {
				status = 429
			}
			assertStatus(t, err, status)
		})
	}
	client := fakeRuntime(t, runtimeEvent{Type: "ready", Protocol: 999, Ready: true})
	_, err := client.run(context.Background(), input{APIKey: "not-sent"}, nil)
	assertStatus(t, err, 503)
}

func TestRealAgnoCapacityAndHealth(t *testing.T) {
	client := startAgnoService(t)
	var closes []func()
	defer func() {
		for _, closeConn := range closes {
			closeConn()
		}
	}()
	for range 4 {
		_, scanner, closeConn, err := client.connect(context.Background(), "run")
		if err != nil {
			t.Fatal(err)
		}
		closes = append(closes, closeConn)
		greeting, err := scanEvent(scanner)
		if err != nil || greeting.Type != "ready" {
			t.Fatal(greeting, err)
		}
	}
	if health := client.health(context.Background()); health.Status != "ready" || health.ActiveRuns != 4 {
		t.Fatal(health)
	}
	_, err := client.run(context.Background(), input{}, nil)
	assertStatus(t, err, 429)
	for _, closeConn := range closes {
		closeConn()
	}
	closes = nil
	until := time.Now().Add(5 * time.Second)
	for client.health(context.Background()).ActiveRuns != 0 && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if health := client.health(context.Background()); health.Status != "ready" || health.ActiveRuns != 0 {
		t.Fatal("slots not released", health)
	}
}
