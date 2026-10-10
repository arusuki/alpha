package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed node_services.sql
var nodeServicesSchema string

const nodeServicesV40 = `CREATE TABLE node_service_mounts (
 container_id TEXT NOT NULL, endpoint TEXT NOT NULL, daemon TEXT NOT NULL,
 owner TEXT NOT NULL, name TEXT NOT NULL, socket_path TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(endpoint,container_id,socket_path)
);
CREATE TRIGGER service_mount_owner BEFORE UPDATE OF owner ON managed_containers
 WHEN OLD.owner<>NEW.owner BEGIN
 SELECT RAISE(ABORT,'请先在节点服务中取消此容器的服务挂载，再修改使用者')
 WHERE EXISTS(SELECT 1 FROM node_service_mounts WHERE container_id=OLD.id);
END;
CREATE TRIGGER service_mount_release BEFORE DELETE ON managed_containers BEGIN
 SELECT RAISE(ABORT,'请先在节点服务中取消此容器的服务挂载，再删除或解除接管')
 WHERE EXISTS(SELECT 1 FROM node_service_mounts WHERE container_id=OLD.id);
END;
`

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
  );` + nodeServicesV40)
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
