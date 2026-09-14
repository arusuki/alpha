package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/protobuf/encoding/protojson"
)

func writeEvents(t *testing.T, path string, events []*tetragon.GetEventsResponse, asArray bool) {
	t.Helper()
	records := make([][]byte, 0, len(events))
	for _, event := range events {
		raw, err := protojson.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, raw)
	}
	body := []byte{}
	if asArray {
		body = append(body, '[')
	}
	for i, record := range records {
		if i > 0 {
			if asArray {
				body = append(body, ',')
			} else {
				body = append(body, '\n')
			}
		}
		body = append(body, record...)
	}
	if asArray {
		body = append(body, ']')
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

// A dump captured with `tetra getevents -o json` must rebuild the same forest
// the live agent would have produced.
func TestReplayRebuildsForestFromJSONDump(t *testing.T) {
	parent := proc("parent", "", "/bin/bash", "-l", "abc123", 100, 0)
	child := proc("child", "parent", "/usr/bin/python", "train.py", "abc123", 200, time.Second)
	shortLived := proc("gone", "parent", "/usr/bin/curl", "http://example", "abc123", 300, 2*time.Second)
	host := proc("sshd", "", "/usr/sbin/sshd", "-D", "", 900, 0)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, path, []*tetragon.GetEventsResponse{
		execEvent(shortLived),
		exitEvent(shortLived),
		execEvent(parent),
		execEvent(child),
		execEvent(host),
	}, false)

	source, err := OpenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	builder := NewBuilder()
	stats, err := Collect(context.Background(), source, builder)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BootstrapOK || stats.Bootstrapped != 0 {
		t.Fatalf("replay reported a bootstrap: %+v", stats)
	}
	if stats.Exec != 4 || stats.Exit != 1 {
		t.Fatalf("exec = %d, exit = %d; want 4 and 1", stats.Exec, stats.Exit)
	}
	forest := builder.Snapshot(Options{})
	container := onlyContainer(t, forest, "abc123")
	if container.ProcessCount != 2 {
		t.Fatalf("process count = %d, want 2", container.ProcessCount)
	}
	if len(container.Roots) != 1 || container.Roots[0].ExecID != "parent" {
		t.Fatalf("roots = %+v, want a single parent root", container.Roots)
	}
	if len(container.Roots[0].Children) != 1 || container.Roots[0].Children[0].ExecID != "child" {
		t.Fatalf("children = %+v, want only the surviving child", container.Roots[0].Children)
	}
}

// The array form must parse as well, since a dump can be assembled by tools
// that wrap the stream in a JSON array.
func TestReplayAcceptsJSONArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	writeEvents(t, path, []*tetragon.GetEventsResponse{
		execEvent(proc("parent", "", "/bin/bash", "", "abc123", 100, 0)),
		execEvent(proc("child", "parent", "/bin/sh", "-c sleep", "abc123", 200, time.Second)),
	}, true)

	source, err := OpenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	builder := NewBuilder()
	if _, err = Collect(context.Background(), source, builder); err != nil {
		t.Fatal(err)
	}
	if len(builder.Snapshot(Options{}).Containers) != 1 {
		t.Fatal("array-form dump did not rebuild the forest")
	}
}

// The committed fixture is what the README tells users to replay, so keep it
// wired to the builder rather than letting it drift.
func TestReplayCommittedFixture(t *testing.T) {
	source, err := OpenReplay("../../tests/fixtures/tetragon-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	builder := NewBuilder()
	stats, err := Collect(context.Background(), source, builder)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Exec != 8 || stats.Exit != 1 {
		t.Fatalf("exec = %d, exit = %d; want 8 and 1", stats.Exec, stats.Exit)
	}
	forest := builder.Snapshot(Options{IncludeHost: true})
	totals := map[string]int{}
	for _, container := range forest.Containers {
		totals[container.ID] = container.ProcessCount
	}
	if len(forest.Containers) != 2 || totals["3f2a9c8b1d4e"] != 3 || totals["b7c1e0f2a9d3"] != 2 {
		t.Fatalf("containers = %d, counts = %v; want 2 containers with 3 and 2 processes", len(forest.Containers), totals)
	}
	if forest.Host == nil || forest.Host.ProcessCount != 2 {
		t.Fatalf("host = %+v, want 2 host processes", forest.Host)
	}
}

func TestReplayRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := OpenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err = Collect(context.Background(), source, NewBuilder()); err == nil {
		t.Fatal("expected a malformed event line to fail collection")
	}
	if _, err := splitEvents([]byte("[1,")); err == nil {
		t.Fatal("expected a malformed array to fail parsing")
	}
	if records, err := splitEvents([]byte("  \n \n")); err != nil || len(records) != 0 {
		t.Fatalf("blank input = %v, %v; want no records and no error", records, err)
	}
}
