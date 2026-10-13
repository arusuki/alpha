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

func upgradeDRAMMounts(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role != "worker" {
		return nil
	}
	_, err := tx.Exec(`ALTER TABLE node_service_mounts ADD COLUMN service TEXT NOT NULL DEFAULT 'rootless-docker' CHECK(service IN ('rootless-docker','dram-bw'));
 UPDATE node_service_settings SET config=json_set(config,'$.control_socket','/run/dram-bw-manager/control.sock','$.socket_path','/run/dram-bw/control.sock') WHERE name='dram-bw';`)
	return err
}
