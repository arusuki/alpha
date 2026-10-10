package containers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestServiceStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
		states    []string
	}{
		{"running and stopped", `{"name":"tetragon","state":"running","detail":"Up 1 hour"}
{"name":"dram-bw","state":"exited","detail":"Exited (1) 2 minutes ago"}`, nil, []string{"running", "exited", "missing"}},
		{"rootless running", `{"name":"rootless-docker","state":"running"}`, nil, []string{"missing", "missing", "running"}},
		{"not deployed", "", nil, []string{"missing", "missing", "missing"}},
		{"restarting", `{"name":"dram-bw","state":"restarting"}`, nil, []string{"missing", "restarting", "missing"}},
		{"paused", `{"name":"tetragon","state":"paused"}`, nil, []string{"paused", "missing", "missing"}},
		{"unrelated", `{"name":"other","state":"running"}`, nil, []string{"missing", "missing", "missing"}},
		{"duplicate", `{"name":"dram-bw","state":"running"}
{"name":"dram-bw","state":"exited"}
{"name":"dram-bw","state":"running"}`, nil, []string{"missing", "unknown", "missing"}},
		{"partial invalid output", `{"name":"tetragon","state":"running"} invalid`, nil, []string{"unknown", "unknown", "unknown"}},
		{"null output", `null`, nil, []string{"unknown", "unknown", "unknown"}},
		{"incomplete output", `{"name":"tetragon"}`, nil, []string{"unknown", "unknown", "unknown"}},
		{"permission denied", "", fmt.Errorf("permission denied"), []string{"unknown", "unknown", "unknown"}},
		{"timeout", "", context.DeadlineExceeded, []string{"unknown", "unknown", "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, cfg := fixture(t)
			h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
				deadline, ok := ctx.Deadline()
				if endpoint != cfg.Endpoint || !ok || time.Until(deadline) > 2*time.Second || input != "" {
					t.Fatalf("query must use configured Docker endpoint with bounded deadline")
				}
				if !strings.Contains(strings.Join(args, " "), "--all --filter label=project-alpha.service") {
					t.Fatalf("query must include stopped services and exclude unrelated containers: %v", args)
				}
				return tc.raw, tc.err
			}
			out := h.ServiceStatuses(context.Background())
			if len(out) != 3 || out[0].Name != "tetragon" || out[1].Name != "dram-bw" || out[2].Name != "rootless-docker" || !reflect.DeepEqual([]string{out[0].State, out[1].State, out[2].State}, tc.states) {
				t.Fatalf("statuses: %+v", out)
			}
			for _, status := range out {
				if status.CheckedAt <= 0 || status.State == "unknown" && status.Detail == "" {
					t.Fatalf("missing observation or failure detail: %+v", status)
				}
			}
		})
	}
}

func TestServiceStatusesPropagateCancellation(t *testing.T) {
	h, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.run = func(ctx context.Context, _ string, _ []string, _ string) (string, error) {
		if ctx.Err() != context.Canceled {
			t.Fatal("request cancellation lost")
		}
		return "", ctx.Err()
	}
	for _, status := range h.ServiceStatuses(ctx) {
		if status.State != "unknown" {
			t.Fatalf("canceled query mistaken for missing service: %+v", status)
		}
	}
}
