package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed container_ownership.sql
var containerOwnershipSQL string

// InstallContainerOwnership keeps the inventory overlay and management records
// consistent. Constraints run inside the writer's SQLite transaction, including
// CLI imports and concurrent ownership edits.
func InstallContainerOwnership(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('owners','managed_containers')").Scan(&count); err != nil {
		return err
	}
	if count != 2 {
		return nil
	}
	_, err := tx.Exec(containerOwnershipSQL)
	return err
}
