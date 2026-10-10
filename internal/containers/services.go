package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"project-alpha/internal/platform"
)

// ServiceStatus reports Docker's live container state, not application readiness.
type ServiceStatus struct {
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Detail    string  `json:"detail,omitempty"`
	CheckedAt float64 `json:"checked_at"`
}

// ServiceStatuses shares the configured local Docker endpoint and the overview's
// request lifetime. No background poller or persisted observations are needed.
func (h *Handler) ServiceStatuses(ctx context.Context) []ServiceStatus {
	checked := platform.Now()
	out := []ServiceStatus{{Name: "tetragon", State: "missing", CheckedAt: checked}, {Name: "dram-bw", State: "missing", CheckedAt: checked}, {Name: "rootless-docker", State: "missing", CheckedAt: checked}}
	fail := func(err error) []ServiceStatus {
		for i := range out {
			out[i].State, out[i].Detail = "unknown", err.Error()
		}
		return out
	}
	cfg, err := h.config()
	if err != nil {
		return fail(fmt.Errorf("服务配置读取失败: %w", err))
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Labels survive Compose project/container name overrides; unrelated user
	// containers are never included in this service inventory.
	raw, err := h.run(ctx, cfg.Endpoint, []string{"container", "ls", "--all", "--filter", "label=project-alpha.service", "--format", `{"name":{{json (.Label "project-alpha.service")}},"state":{{json .State}},"detail":{{json .Status}}}`}, "")
	if err != nil {
		return fail(err)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	seen := map[string]bool{}
	for {
		var row ServiceStatus
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return fail(fmt.Errorf("Docker 服务状态格式无效: %w", err))
		}
		if row.Name == "" || row.State == "" {
			return fail(fmt.Errorf("Docker 服务状态缺少 name 或 state"))
		}
		for i := range out {
			if out[i].Name != row.Name {
				continue
			}
			if seen[row.Name] {
				out[i].State, out[i].Detail = "unknown", "发现多个同名服务容器，请检查部署"
				continue
			}
			seen[row.Name] = true
			row.CheckedAt = checked
			out[i] = row
		}
	}
	return out
}
