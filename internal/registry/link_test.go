package registry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"project-alpha/internal/platform"
)

func startLink(t *testing.T, address, token, id, registryID string, dispatch Dispatch) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Connect(ctx, LinkConfig{Address: address, Token: token, ControlID: id, RegistryID: registryID}, dispatch, nil)
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}
func awaitLink(t *testing.T, hub *Hub) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := hub.Call(ctx, Request{Action: "ping"})
		cancel()
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("control did not establish outbound link")
}
func TestRegistryPinsFirstAuthenticatedControlAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	if _, err = db.CheckMode("registry"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CheckMode("control"); err == nil {
		t.Fatal("registry accepted control mode")
	}
	registryID, err := db.CheckMode("registry")
	if err != nil {
		t.Fatal(err)
	}
	token, id := strings.Repeat("s", 64), strings.Repeat("a", 32)
	server := NewServer(db, "Abcd1234", token, nil, false)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	t.Cleanup(server.Hub.Close)
	dial := func(secret, identity, target, proto string) error {
		endpoint, _ := Endpoint(httpServer.URL)
		config, _ := websocket.NewConfig(endpoint, httpServer.URL)
		config.Protocol = []string{proto}
		config.Header.Set("Authorization", "Bearer "+secret)
		config.Header.Set("X-Alpha-Control", identity)
		config.Header.Set("X-Alpha-Registry", target)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn, err := config.DialContext(ctx)
		if conn != nil {
			conn.Close()
		}
		return err
	}
	if err = dial("wrong", id, registryID, protocol); err == nil {
		t.Fatal("unauthenticated control accepted")
	}
	if err = dial(token, id, registryID, "old-protocol"); err == nil {
		t.Fatal("wrong protocol accepted")
	}
	if err = dial(token, id, strings.Repeat("f", 32), protocol); err == nil {
		t.Fatal("unexpected registry instance accepted")
	}
	var count int
	if err = db.SQL.QueryRow("SELECT count(*) FROM registry_control").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid handshake pinned control: %d %v", count, err)
	}
	stop := startLink(t, httpServer.URL, token, id, registryID, func(context.Context, Request) (any, error) { return map[string]bool{"ok": true}, nil })
	awaitLink(t, server.Hub)
	if err = dial(token, strings.Repeat("b", 32), registryID, protocol); err == nil {
		t.Fatal("another authenticated control replaced binding")
	}
	awaitLink(t, server.Hub)
	stop()
	server.Hub.Close()
	httpServer.Close()
	db.SQL.Close()
	db, err = platform.OpenDatabase(directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	if err = bindControl(db, strings.Repeat("b", 32)); err == nil {
		t.Fatal("restart forgot control binding")
	}
	server = NewServer(db, "Abcd1234", token, nil, false)
	httpServer = httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	t.Cleanup(server.Hub.Close)
	startLink(t, httpServer.URL, token, id, registryID, func(context.Context, Request) (any, error) { return nil, nil })
	awaitLink(t, server.Hub)
}
func TestRegistryEndpointRejectsPublicPlaintextAndCredentials(t *testing.T) {
	for _, value := range []string{"http://registry.example.com", "https://user:pass@registry.example.com", "https://registry.example.com/other", "https://registry.example.com?token=x", "wss://registry.example.com"} {
		if _, err := Endpoint(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	for _, value := range []string{"https://registry.example.com", "http://127.0.0.1:1234"} {
		if _, err := Endpoint(value); err != nil {
			t.Errorf("rejected %q: %v", value, err)
		}
	}
}
