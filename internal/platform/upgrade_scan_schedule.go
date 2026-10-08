package platform

import "database/sql"

func upgradeScanSchedule(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role != "worker" {
		return nil
	}
	_, err := tx.Exec(`ALTER TABLE settings ADD COLUMN schedule_last_run REAL NOT NULL DEFAULT 0;
UPDATE settings SET value=json_set(value,
 '$.schedule_mode',CASE WHEN json_extract(value,'$.interval_minutes')>0 THEN 'interval' ELSE 'off' END,
 '$.schedule_times',json('[]'),'$.schedule_weekdays',json('[0,1,2,3,4,5,6]'),
 '$.schedule_timezone','UTC','$.retain_records',0), revision=revision+1,
 schedule_last_run=coalesce((SELECT max(created_at) FROM jobs WHERE trigger='scheduled'),0);`)
	return err
}
