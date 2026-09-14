package app

import (
	"database/sql"
	"net/http"

	"project-alpha/internal/agent"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/storage"
)

// Modules composes independently owned APIs behind the platform's auth guards.
type Modules struct{ Storage, Agent, Process platform.Module }

func (m Modules) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if agent.IsRoute(r.URL.Path) {
		return m.Agent.Dispatch(w, r, user)
	}
	if process.IsRoute(r.URL.Path) {
		return m.Process.Dispatch(w, r, user)
	}
	return m.Storage.Dispatch(w, r, user)
}

// Initialize creates all module tables atomically for a new data directory.
func Initialize(tx *sql.Tx) error {
	if err := storage.Initialize(tx); err != nil {
		return err
	}
	return agent.Initialize(tx)
}
