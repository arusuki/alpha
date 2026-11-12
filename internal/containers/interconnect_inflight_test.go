package containers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

func emptyLinkConfig() link.Config {
	return link.Config{Protocol: link.Protocol, Node: strings.Repeat("b", 32), Revision: 1, Pool: netip.MustParsePrefix("10.240.0.0/16"), Subnet: netip.MustParsePrefix("10.240.0.0/24"), Internal: netip.MustParseAddr("192.0.2.1")}
}

func TestInterconnectInFlightHTTPApplyAndIdempotence(t *testing.T) {
	h, _, _ := fixture(t)
	c := emptyLinkConfig()
	if _, err := h.db.SQL.Exec("INSERT INTO service_identity VALUES(1,'worker',?)", c.Node); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	finish := func() { unblock.Do(func() { close(release) }) }
	var guards, applies atomic.Int64
	h.linkHelper = func(ctx context.Context, req link.Request) (link.Status, error) {
		if req.Operation == "guard" && guards.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return link.Status{}, ctx.Err()
			}
		}
		if req.Operation == "apply" {
			applies.Add(1)
		}
		return link.Status{Protocol: link.Protocol, Applied: req.Config.Revision}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, value, err := h.Dispatch(w, r, admin)
		if err != nil {
			status = 500
			var apiErr *httpapi.Error
			if errors.As(err, &apiErr) {
				status = apiErr.Status
			}
			value = map[string]string{"error": err.Error()}
		}
		httpapi.WriteJSON(w, status, value)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(finish)
	post := func(client *http.Client, req link.Request) (*http.Response, error) {
		return client.Post(server.URL+link.WorkerPath, "application/json", strings.NewReader(httpapi.JSONText(req)))
	}
	invalid := c
	invalid.Revision = 0
	r, err := post(&http.Client{Timeout: time.Second}, link.Request{Operation: "apply", Config: invalid})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 409 {
		t.Fatal("an idle worker mistook an invalid zero revision for in-flight work", r.StatusCode)
	}
	timedOut := make(chan error, 1)
	go func() {
		r, err := post(&http.Client{Timeout: 200 * time.Millisecond}, link.Request{Operation: "apply", Config: c})
		if r != nil {
			r.Body.Close()
		}
		timedOut <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("slow helper did not start")
	}
	if err := <-timedOut; err == nil {
		t.Fatal("slow response did not exceed the real HTTP client timeout")
	}
	client := &http.Client{Timeout: time.Second}
	state := func() (int64, int64, int64, string) {
		t.Helper()
		r, err := client.Get(server.URL + link.WorkerPath)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var saved struct {
			Desired, Applied, Applying int64
			Error                      string
			Digest                     string `json:"config_digest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&saved); err != nil || r.StatusCode != 200 {
			t.Fatal(saved, err, r.StatusCode)
		}
		if want := fmt.Sprintf("%x", sha256.Sum256([]byte(httpapi.JSONText(c)))); saved.Digest != want {
			t.Fatal("worker status did not identify the exact persisted policy", saved.Digest, want)
		}
		return saved.Desired, saved.Applied, saved.Applying, saved.Error
	}
	if desired, applied, applying, failure := state(); desired != c.Revision || applied != 0 || applying != c.Revision || failure != "正在应用" {
		t.Fatal(desired, applied, applying, failure)
	}
	other := c
	other.Internal = netip.MustParseAddr("192.0.2.2")
	for _, req := range []link.Request{{Operation: "apply", Config: c}, {Operation: "apply", Config: c, Force: true}, {Operation: "apply", Config: other}} {
		r, err := post(client, req)
		if err != nil {
			t.Fatal("duplicate request queued behind the slow helper:", err)
		}
		r.Body.Close()
		if r.StatusCode != 503 {
			t.Fatal("in-flight apply was acknowledged or queued:", r.StatusCode)
		}
	}
	finish()
	deadline := time.Now().Add(3 * time.Second)
	for {
		desired, applied, applying, failure := state()
		if desired == c.Revision && applied == c.Revision && applying == 0 && failure == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not settle after the original request ended", desired, applied, applying, failure)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, req := range []link.Request{{Operation: "apply", Config: c}, {Operation: "apply", Config: other}} {
		r, err := post(client, req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		want := 200
		if req.Config.Internal != c.Internal {
			want = 409
		}
		if r.StatusCode != want {
			t.Fatal("completed version was not checked against the exact payload:", r.StatusCode, want)
		}
	}
	if guards.Load() != 1 || applies.Load() != 1 {
		t.Fatal("duplicates repeated external effects:", guards.Load(), applies.Load())
	}
	r, err = post(client, link.Request{Operation: "apply", Config: c, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 || applies.Load() != 2 {
		t.Fatal("explicit force did not replay the completed policy", r.StatusCode, applies.Load())
	}
}

func TestInterconnectCanceledQueuedApplyDoesNotStart(t *testing.T) {
	h, _, _ := fixture(t)
	c := emptyLinkConfig()
	if _, err := h.db.SQL.Exec("INSERT INTO service_identity VALUES(1,'worker',?)", c.Node); err != nil {
		t.Fatal(err)
	}
	var helpers atomic.Int64
	h.linkHelper = func(_ context.Context, req link.Request) (link.Status, error) {
		helpers.Add(1)
		return link.Status{Protocol: link.Protocol, Applied: req.Config.Revision}, nil
	}
	h.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", link.WorkerPath, bytes.NewBufferString(httpapi.JSONText(link.Request{Operation: "apply", Config: c}))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		_, _, err := h.Dispatch(httptest.NewRecorder(), r, admin)
		result <- err
	}()
	cancel()
	h.mu.Unlock()
	var apiErr *httpapi.Error
	if err := <-result; !errors.As(err, &apiErr) || apiErr.Status != 503 || helpers.Load() != 0 {
		t.Fatal("canceled queued apply started external effects", err, helpers.Load())
	}
	if _, err := h.linkApply(context.Background(), c, "admin"); err != nil {
		t.Fatal("a later retry could not apply the intention", err)
	}
}

func TestInterconnectInterruptedApplyIsNotInFlightAfterRestart(t *testing.T) {
	h, _, _ := fixture(t)
	c := emptyLinkConfig()
	if _, err := h.db.SQL.Exec("UPDATE link_local SET desired=?,config=?,error='正在应用' WHERE id=1", c.Revision, httpapi.JSONText(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL.Exec("INSERT INTO link_operations(revision,at,result) VALUES(?,?,'pending')", c.Revision, platform.Now()); err != nil {
		t.Fatal(err)
	}
	recovered := NewHandler(h.db)
	var applies int
	recovered.linkHelper = func(_ context.Context, req link.Request) (link.Status, error) {
		if req.Operation == "apply" {
			applies++
		}
		return link.Status{Protocol: link.Protocol, Applied: req.Config.Revision}, nil
	}
	recovered.restoreLinks(context.Background(), "", "", "startup")
	var applied, interrupted int64
	if err := h.db.SQL.QueryRow("SELECT applied FROM link_local").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM link_operations WHERE result='interrupted'").Scan(&interrupted); err != nil {
		t.Fatal(err)
	}
	if applied != c.Revision || applies != 1 || interrupted != 1 || recovered.linkApplying.Load() != 0 {
		t.Fatal("interrupted durable intent remained stuck as in-flight", applied, applies, interrupted, recovered.linkApplying.Load())
	}
}
