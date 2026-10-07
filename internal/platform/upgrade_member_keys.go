package platform

import "database/sql"

// Upgrade in place: existing single-key text is already a one-line key list.
// Durable revocations and worker sync records survive failed or interrupted edits.
func upgradeMemberKeys(tx *sql.Tx) error {
	tables, err := Rows(tx, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return err
	}
	for _, table := range tables {
		switch table["name"] {
		case "member_access":
			if _, err = tx.Exec(`CREATE TABLE member_key_revocations (
 member_id TEXT NOT NULL REFERENCES members(id), public_key TEXT NOT NULL,
 PRIMARY KEY(member_id,public_key));`); err != nil {
				return err
			}
		case "member_work":
			if _, err = tx.Exec(`CREATE TABLE member_key_sync (
 member_id TEXT NOT NULL REFERENCES members(id), node_id TEXT NOT NULL REFERENCES cluster_nodes(id),
 state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', PRIMARY KEY(member_id,node_id));`); err != nil {
				return err
			}
		}
	}
	return nil
}
