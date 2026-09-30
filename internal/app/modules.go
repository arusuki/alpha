package app

import (
	"database/sql"
	"net/http"

	"project-alpha/internal/containers"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/storage"
)

// Modules composes node operations behind the worker authentication boundary.
type Modules struct {
	Storage, Process, Containers platform.Module
}

func (m Modules) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if containers.IsRoute(r.URL.Path) && m.Containers != nil {
		return m.Containers.Dispatch(w, r, user)
	}
	if process.IsRoute(r.URL.Path) {
		return m.Process.Dispatch(w, r, user)
	}
	return m.Storage.Dispatch(w, r, user)
}

// Initialize creates node tables and binds the directory to worker mode atomically.
func Initialize(tx *sql.Tx) error {
	if err := storage.Initialize(tx); err != nil {
		return err
	}
	if err := containers.Initialize(tx); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO service_identity VALUES(1,'worker',?)", platform.RandomHex(16))
	return err
}
