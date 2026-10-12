package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed rootless_owners.sql
var rootlessOwnersSchema string

// ContainerOwnersSQL is the live view shared by GPU, disk snapshots and inventory.
// Quota checks still query owners alone, never this expanded view.
const ContainerOwnersSQL = `SELECT container_id,owner FROM owners
 UNION ALL SELECT r.container_id,o.owner FROM rootless_container_owners r JOIN owners o ON o.container_id=r.owner_container_id`

// Rootless children refer to a primary container, never occupy its owner slot,
// and follow its current ownership (including an explicit unassignment).
func InstallRootlessOwners(tx *sql.Tx) error {
	_, err := tx.Exec(rootlessOwnersSchema)
	return err
}

func upgradeRootlessOwners(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "worker" {
		return InstallRootlessOwners(tx)
	}
	return nil
}
