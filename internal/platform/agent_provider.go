package platform

import "database/sql"

func upgradeAgentProvider(tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='agent_settings')").Scan(&exists); err != nil {
		return err
	}
	// Current controls and older worker data directories can own this table.
	if !exists {
		return nil
	}
	// Provider selection is explicit; do not infer it from an endpoint URL.
	// Keep credentials and every existing setting, invalidating stale editors.
	_, err := tx.Exec(`UPDATE agent_settings SET value=json_set(value,'$.provider','openai'),revision=revision+1 WHERE id=1`)
	return err
}
