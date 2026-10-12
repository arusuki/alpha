package containerorigin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveCurrentOwnership(t *testing.T) {
	parents := map[string]string{"child": "parent", "parent": "member", "cycle-a": "cycle-b", "cycle-b": "cycle-a"}
	for _, tc := range []struct {
		id     string
		owners map[string]string
		want   string
	}{
		{"child", map[string]string{"member": "alice"}, "alice"},
		{"child", map[string]string{"member": "alice", "child": "bob"}, "bob"},
		{"child", map[string]string{"member": "alice", "parent": ""}, ""},
		{"child", nil, ""}, {"missing", nil, ""}, {"cycle-a", nil, ""},
	} {
		if got := Resolve(tc.id, tc.owners, parents); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.id, got, tc.want)
		}
	}
}

func TestReadParentsBatchesAndRejectsUnrequestedRecords(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "origin-client-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	parent := strings.Repeat("f", 64)
	var calls atomic.Int32
	var bad atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		ids := r.URL.Query()["container_id"]
		if len(ids) > 128 || len(ids) == 0 {
			t.Error("invalid batch", len(ids))
		}
		result := Response{Version: Version, Origins: []Record{}}
		for _, id := range ids {
			result.Origins = append(result.Origins, Record{Version: Version, ContainerID: id, Source: Source{PID: 123, StartTicks: 1, ContainerID: parent}})
		}
		if bad.Load() {
			result.Origins[0].ContainerID = parent
		}
		json.NewEncoder(w).Encode(result)
	})}
	go server.Serve(listener)
	defer server.Close()
	ids := []string{}
	for i := range 260 {
		ids = append(ids, fmt.Sprintf("%064x", i))
	}
	ids = append(ids, ids[0])
	parents, err := ReadParents(context.Background(), socket, ids)
	if err != nil || len(parents) != 260 || calls.Load() != 3 {
		t.Fatalf("batching: calls=%d parents=%d err=%v", calls.Load(), len(parents), err)
	}
	// Synchronization through HTTP completion ensures the handler has returned.
	bad.Store(true)
	if parents, err = ReadParents(context.Background(), socket, ids[:1]); err == nil || parents != nil {
		t.Fatal("accepted an unsolicited record")
	}
}
