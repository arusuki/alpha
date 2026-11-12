package platform

import (
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed interconnect_control.sql
var linkControlSchema string

//go:embed interconnect_worker.sql
var linkWorkerSchema string

func InstallInterconnect(tx *sql.Tx, role string) error {
	var schema string
	switch role {
	case "control":
		schema = linkControlSchema
	case "worker":
		schema = linkWorkerSchema
	case "registry":
		return nil
	default:
		return fmt.Errorf("unknown interconnect role %q", role)
	}
	_, err := tx.Exec(schema)
	return err
}
func upgradeInterconnect(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	return InstallInterconnect(tx, role)
}
