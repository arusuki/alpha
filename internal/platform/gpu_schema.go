package platform

import "database/sql"

// GPU history belongs to workers. Intervals are bounded by the collector and
// expired on every collection, including collection failures.
func InstallGPUHistory(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE gpu_intervals (
 gpu_uuid TEXT NOT NULL, name TEXT NOT NULL, started_at REAL NOT NULL,
 ended_at REAL NOT NULL CHECK(ended_at>started_at), utilization REAL,
 owners TEXT NOT NULL, PRIMARY KEY(gpu_uuid,ended_at));
 CREATE INDEX gpu_intervals_expiry ON gpu_intervals(ended_at);`)
	return err
}

func upgradeGPUHistory(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "worker" {
		return InstallGPUHistory(tx)
	}
	return nil
}
