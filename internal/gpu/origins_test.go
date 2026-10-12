package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/containerorigin"
)

func originEndpoint(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gpu-origin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := dir + "/control.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestGPUUsesAuditedCreationSource(t *testing.T) {
	child, parent, other := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, tc := range []struct {
		name, sourceError, sourceContainer string
		registered                         bool
		want                               string
	}{
		{"registered parent", "", parent, true, "alice"},
		{"unregistered parent", "", parent, false, rootlessOwner},
		{"unreadable source", "process gone", parent, true, rootlessOwner},
		{"host client", "", "", true, rootlessOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := originEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != containerorigin.Endpoint || !reflect.DeepEqual(r.URL.Query()["container_id"], []string{child, other}) {
					t.Error(r.URL.String())
				}
				json.NewEncoder(w).Encode(containerorigin.Response{Version: 1, Origins: []containerorigin.Record{{Version: 1, ContainerID: child, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), BootID: "boot", Source: containerorigin.Source{PID: 123, UID: 1000, StartTicks: 4500, ContainerID: tc.sourceContainer, Error: tc.sourceError}}}})
			})
			devices := []Device{{Processes: []Process{{ContainerID: child, Owner: rootlessOwner}, {ContainerID: child, Owner: rootlessOwner}, {ContainerID: other, Owner: rootlessOwner}}}}
			owners := map[string]string{}
			if tc.registered {
				owners[parent] = "alice"
			}
			if warning := readContainerOrigins(context.Background(), devices, owners, socket); warning != "" {
				t.Fatal(warning)
			}
			for _, p := range devices[0].Processes[:2] {
				if p.Owner != tc.want {
					t.Fatal(p)
				}
			}
			if devices[0].Processes[2].Owner != rootlessOwner {
				t.Fatal("guessed old unaudited container")
			}
			applyContainerOwners(devices, nil, map[string]string{child: "explicit-owner"})
			if devices[0].Processes[0].Owner != "explicit-owner" {
				t.Fatal("audit overrode explicit ownership")
			}
		})
	}
}

func TestGPURejectsInvalidAuditResponses(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{404, `unknown endpoint`}, {500, `damaged provenance`}, {200, `{"version":2,"origins":[]}`}, {200, `{"version":1,"origins":null}`}, {200, `{"version":1,"origins":[]} {}`}, {200, `{"version":1,"origins":[{"version":1,"container_id":"bad"}]}`},
	} {
		t.Run(fmt.Sprint(tc.status)+tc.body, func(t *testing.T) {
			socket := originEndpoint(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			devices := []Device{{Processes: []Process{{ContainerID: id, Owner: rootlessOwner}}}}
			if warning := readContainerOrigins(context.Background(), devices, map[string]string{}, socket); warning == "" {
				t.Fatal("missing warning")
			}
			if devices[0].Processes[0].Owner != rootlessOwner {
				t.Fatal("guessed unavailable provenance")
			}
		})
	}
}
