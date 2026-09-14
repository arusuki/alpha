package process

import (
	"net/http"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Handler exposes the in-memory forest over the platform's authenticated API.
// Watcher is nil when no Tetragon agent is available, in which case the route
// reports that monitoring is unavailable instead of serving an empty forest.
type Handler struct {
	Watcher *Watcher
}

func NewHandler(watcher *Watcher) *Handler { return &Handler{Watcher: watcher} }

// IsRoute reports whether path belongs to this module, mirroring agent.IsRoute.
func IsRoute(path string) bool { return strings.HasPrefix(path, "/api/process/") }

// Dispatch serves the read-only forest. Every authenticated user may view it,
// matching the read model of the storage module.
func (s *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if r.URL.Path != "/api/process/forest" || r.Method != "GET" {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	if s.Watcher == nil {
		return 0, nil, httpapi.NewError(503, "未启用容器进程监控")
	}
	forest, status := s.Watcher.Snapshot(r.URL.Query().Get("host") == "1")
	return 200, map[string]any{"status": status, "forest": forest}, nil
}
