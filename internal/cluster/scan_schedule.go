package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

type scheduleResult struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (h *Control) distributeScanSchedule(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	input, err := httpapi.RequestBody(w, r)
	if err != nil {
		return 0, nil, err
	}
	var ids []string
	if len(input) != 2 || json.Unmarshal(input["node_ids"], &ids) != nil || len(ids) == 0 || len(ids) > 128 {
		return 0, nil, httpapi.NewError(400, "请选择 1–128 个 worker 并提供扫描计划")
	}
	plan, err := storage.ParseSchedule(input["schedule"])
	if err != nil {
		return 0, nil, err
	}
	// Validate the entire target set before writing to any worker.
	nodes := make([]Node, len(ids))
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !identifier.MatchString(id) || seen[id] {
			return 0, nil, httpapi.NewError(400, "worker 标识无效或重复")
		}
		seen[id] = true
		nodes[i], err = h.node(id)
		if err != nil {
			return 0, nil, err
		}
		if nodes[i].Kind != "worker" {
			return 0, nil, httpapi.NewError(400, "扫描计划只能下发到 worker")
		}
	}
	results := make([]scheduleResult, len(nodes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := scheduleResult{ID: n.ID, Name: n.Name}
			defer func() { results[i] = result }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-r.Context().Done():
				result.Error = "下发已中断，请重试并核实节点配置"
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if err := h.applyScanSchedule(ctx, n, user, plan); err != nil {
				result.Error = err.Error()
				return
			}
			result.OK = true
		}()
	}
	wg.Wait()
	return 200, map[string]any{"results": results}, nil
}

func (h *Control) applyScanSchedule(ctx context.Context, n Node, user platform.User, plan storage.Schedule) error {
	info, err := h.probe(ctx, n)
	if err != nil {
		return err
	}
	if info.ID != n.ID || info.Protocol != Protocol {
		return httpapi.NewError(409, "节点身份或业务协议不一致，请核对节点并统一版本")
	}
	var settings storage.Settings
	if err := h.call(ctx, n, "GET", "/api/settings", nil, user, &settings); err != nil {
		return err
	}
	plan.Apply(&settings.Value)
	// Keep the revision read above: concurrent edits must produce a conflict,
	// never silently overwrite another administrator's changes.
	var saved storage.Settings
	return h.call(ctx, n, "PUT", "/api/settings", settings, user, &saved)
}
