package platform

import "database/sql"

func upgradeScheduledAgent(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	var err error
	switch role {
	case "worker":
		_, err = tx.Exec(`ALTER TABLE settings ADD COLUMN analysis_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_status TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN analysis_error TEXT NOT NULL DEFAULT '';
UPDATE settings SET value=json_set(value,'$.auto_agent_analyze',json('false')),revision=revision+1;
UPDATE jobs SET config=json_set(config,'$.auto_agent_analyze',json('false'));`)
	case "control":
		_, err = tx.Exec(`ALTER TABLE agent_sessions ADD COLUMN scheduled_job_id TEXT;
CREATE UNIQUE INDEX idx_agent_scheduled ON agent_sessions(node_id,scheduled_job_id,report_scope) WHERE scheduled_job_id IS NOT NULL;`)
	}
	return err
}
