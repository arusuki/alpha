package rootless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testEventStream struct {
	filters map[string][]string
	send    chan string
	drop    chan struct{}
	done    chan struct{}
}

func eventServer(t *testing.T, socket string) chan *testEventStream {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	streams := make(chan *testEventStream, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := &testEventStream{send: make(chan string), drop: make(chan struct{}), done: make(chan struct{})}
		defer close(stream.done)
		if r.URL.Path != "/events" || r.URL.Query().Get("since") != "" || json.Unmarshal([]byte(r.URL.Query().Get("filters")), &stream.filters) != nil {
			t.Error("invalid event subscription", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		streams <- stream
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stream.drop:
				return
			case event := <-stream.send:
				fmt.Fprintln(w, event)
				w.(http.Flusher).Flush()
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return streams
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event stream")
		var zero T
		return zero
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("event stream did not reach expected state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventStreamsOwnRecoveryAndStopWithLastBinding(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rootless-events-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m, u := testManager(t)
	u.Home = dir
	if err = os.MkdirAll(layout(u).Run, 0700); err != nil {
		t.Fatal(err)
	}
	rootlessStreams := eventServer(t, layout(u).Socket)
	host := "unix://" + dir + "/host.sock"
	hostStreams := eventServer(t, dir+"/host.sock")
	source, err := socketIdentity(layout(u).Socket)
	if err != nil {
		t.Fatal(err)
	}
	var inspections atomic.Int32
	m.run = func(args, _ []string, _ time.Duration, _ bool) (result, error) {
		switch {
		case strings.Contains(strings.Join(args, " "), "container ls"):
			return result{Out: "target\n"}, nil
		case strings.Contains(strings.Join(args, " "), "container inspect"):
			inspections.Add(1)
			return result{Out: `[{"Id":"target","State":{"Running":false}}]`}, nil
		default:
			return result{}, errors.New("unexpected CLI call")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{manager: m, user: u, ctx: ctx, bootID: "boot", stateFile: dir + "/bindings.json",
		store: bindingStore{Version: 1, Bindings: []binding{{Host: host, Container: "target", Name: "target", SocketPath: "/sock",
			BootID: "boot", PID: 42, StartedAt: "first", Receipt: &mountReceipt{Namespace: 1, Device: uint64(source.Dev), Inode: source.Ino, MountID: "2"}}}}}
	t.Cleanup(func() { cancel(); d.wg.Wait() })
	d.mu.Lock()
	d.syncWatchers()
	d.mu.Unlock()
	rootless := receive(t, rootlessStreams)
	target := receive(t, hostStreams)
	if fmt.Sprint(rootless.filters["type"]) != "[daemon]" || fmt.Sprint(target.filters["event"]) != "[start unpause die destroy]" {
		t.Fatal("subscriptions include unnecessary events", rootless.filters, target.filters)
	}
	eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.watchers[host].ready && d.watchers["unix://"+layout(u).Socket].ready
	})
	if inspections.Load() != 1 {
		t.Fatal("duplicate initial reconciliation", inspections.Load())
	}
	before, err := os.Stat(d.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	// Queue an unrelated event followed by a matching one as a processing
	// barrier. Neither changes the saved stopped-container error.
	target.send <- `{"Action":"start","Actor":{"ID":"unrelated"}}`
	target.send <- `{"Action":"unpause","Actor":{"ID":"target"}}`
	eventually(t, func() bool { return inspections.Load() == 2 })
	d.mu.Lock()
	after, err := os.Stat(d.stateFile)
	d.mu.Unlock()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unchanged/unrelated event rewrote state", err)
	}
	// A disconnected target stream must inspect once after reconnecting.
	close(target.drop)
	target = receive(t, hostStreams)
	eventually(t, func() bool { return inspections.Load() == 3 })
	// Rootless recovery with an unchanged source does not repeat that work.
	d.mu.Lock()
	if err = d.reconcileSocket(); err != nil {
		t.Error(err)
	}
	if inspections.Load() != 3 {
		t.Error("unchanged socket caused reconciliation")
	}
	// A new source inode must retry affected bindings even without a target
	// container event (e.g. an external systemctl restart).
	d.store.Bindings[0].Receipt.Inode++
	_ = d.reconcileSocket() // The stopped target still has an explicit error.
	if inspections.Load() != 4 {
		t.Error("changed socket did not trigger recovery")
	}
	d.mu.Unlock()
	r := httptest.NewRequest("POST", controlPath, strings.NewReader(fmt.Sprintf(`{"args":["remove","target","--host",%q,"--socket-path","/sock"]}`, host)))
	w := httptest.NewRecorder()
	d.control(w, r)
	var response controlResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error != "" {
		t.Fatal("remove failed", w.Code, w.Body.String())
	}
	receive(t, target.done)
	receive(t, rootless.done)
	d.wg.Wait()
	if len(d.watchers) != 0 {
		t.Fatal("last binding left subscriptions running")
	}
}

func TestUnrelatedEventDoesNotCreateStateAndFailedSaveRetries(t *testing.T) {
	dir := t.TempDir()
	d := &daemon{stateFile: dir + "/missing/bindings.json", store: bindingStore{Version: 1, Bindings: []binding{}}}
	if err := d.reconcile("unix:///host.sock", "unrelated"); err != nil {
		t.Fatal("unrelated event tried to save", err)
	}
	if err := d.save(); err == nil {
		t.Fatal("expected failed write")
	}
	if err := os.Mkdir(dir+"/missing", 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.save(); err != nil {
		t.Fatal("failed save was not retried", err)
	}
	if _, err := loadBindings(d.stateFile); err != nil {
		t.Fatal("retry did not persist state", err)
	}
}

func TestFailedEventSubscriptionDoesNotReconcile(t *testing.T) {
	m, u := testManager(t)
	m.run = func([]string, []string, time.Duration, bool) (result, error) {
		t.Error("reconciled before successful subscription")
		return result{}, nil
	}
	d := &daemon{manager: m, user: u}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	watcher := &eventWatcher{}
	err := d.consumeEvents(context.Background(), server.Client(), server.URL, testHost, false, watcher)
	if err == nil || !strings.Contains(err.Error(), "503") || watcher.ready {
		t.Fatal("failed subscription treated as connected", err)
	}
}
