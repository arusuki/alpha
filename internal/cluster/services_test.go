package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"project-alpha/internal/containers"
)

func TestOverviewIncludesLiveServices(t *testing.T) {
	f := setup(t)
	w, server := worker(t, strings.Repeat("a", 32), Inventory{Containers: []Container{}}, nil)
	w.Services = func(ctx context.Context) []containers.ServiceStatus {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		return []containers.ServiceStatus{{Name: "dram-bw", State: "unknown", Detail: "Docker unavailable", CheckedAt: 123}}
	}
	add(t, f, w, server, "worker")
	response := f.request(t, "GET", "/api/cluster/overview", nil)
	requireStatus(t, response, 200)
	var result struct {
		Nodes   []NodeStatus
		Partial bool
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || !result.Nodes[0].Online || result.Partial || result.Nodes[0].Inventory == nil {
		t.Fatalf("service failure must not hide node inventory: %s", response.Body.String())
	}
	services := result.Nodes[0].Inventory.Services
	if len(services) != 1 || services[0].State != "unknown" || services[0].Detail != "Docker unavailable" {
		t.Fatalf("service observation not forwarded: %+v", services)
	}
}
