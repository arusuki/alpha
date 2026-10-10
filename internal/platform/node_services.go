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

func upgradeNodeServices(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "worker" {
		// The historical step must produce the v40 schema, before parameters exist.
		_, err := tx.Exec(`CREATE TABLE node_service_settings (
   name TEXT PRIMARY KEY CHECK(name IN ('tetragon','dram-bw','rootless-docker')),
   config TEXT NOT NULL CHECK(json_valid(config))
  );` + nodeServicesSchema)
		return err
	}
	return nil
}

func upgradeServiceParameters(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role != "worker" {
		return nil
	}
	if _, err := tx.Exec("ALTER TABLE node_service_settings RENAME TO node_service_settings_v40"); err != nil {
		return err
	}
	if _, err := tx.Exec(nodeServiceSettingsSchema); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO node_service_settings(name,config)
 SELECT name, CASE WHEN name='dram-bw' THEN json_set(config,'$.dram',json('{"backend":"amd-rome","interval_us":100000,"peak_gbps":0}')) ELSE config END
 FROM node_service_settings_v40;
 DROP TABLE node_service_settings_v40;`)
	return err
}
