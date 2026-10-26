package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed node_services.sql
var nodeServicesSchema string

//go:embed node_service_settings.sql
var nodeServiceSettingsSchema string

func InstallNodeServices(tx *sql.Tx) error {
	_, err := tx.Exec(nodeServiceSettingsSchema + nodeServicesSchema)
	return err
}
