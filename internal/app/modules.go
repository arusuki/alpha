package app

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"project-alpha/internal/containers"
	"project-alpha/internal/gpu"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/storage"
)

// Modules composes node operations behind the worker authentication boundary.
type Modules struct {
	Storage, Process, Containers, GPU platform.Module
}

func (m Modules) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if gpu.IsRoute(r.URL.Path) && m.GPU != nil {
		return m.GPU.Dispatch(w, r, user)
	}
	if containers.IsRoute(r.URL.Path) && m.Containers != nil {
		return m.Containers.Dispatch(w, r, user)
	}
	if process.IsRoute(r.URL.Path) {
		return m.Process.Dispatch(w, r, user)
	}
	status, value, err := m.Storage.Dispatch(w, r, user)
	if err == nil && r.URL.Path == "/api/owners" && r.Method == "PUT" {
		if services, ok := m.Containers.(interface{ ReconcileServices(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()
			if err := services.ReconcileServices(ctx); err != nil {
				value.(map[string]any)["warning"] = "容器归属已保存，服务挂载尚未完成，请在节点服务中重试应用：" + err.Error()
			}
		}
	}
	return status, value, err
}

// Initialize creates node tables and binds the directory to worker mode atomically.
func Initialize(tx *sql.Tx) error {
	if err := platform.InstallGPUHistory(tx); err != nil {
		return err
	}
	if err := storage.Initialize(tx); err != nil {
		return err
	}
	if err := containers.Initialize(tx); err != nil {
		return err
	}
	if err := platform.InstallContainerOwnership(tx); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO service_identity VALUES(1,'worker',?)", platform.RandomHex(16))
	return err
}
