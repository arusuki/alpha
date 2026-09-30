package agent

import (
	"database/sql"
	_ "embed"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type object = map[string]any

// Store owns model settings and analysis conversations.
type Store struct {
	*platform.Database
	NodeID string
}

func NewStore(db *platform.Database) *Store { return &Store{Database: db} }

// Recover runs once at service startup, before any managers are created.
func Recover(db *platform.Database) error {
	return db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE agent_sessions SET status='interrupted',error='服务重启，分析已中断，可继续提问',active_job_id=NULL,updated_at=? WHERE status IN ('queued','scanning','running','cancelling')", platform.Now()); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE agent_cleanup_entries SET status='uncertain',error='服务重启，删除结果未确认；请检查实际路径后重新扫描' WHERE status='deleting'")
		return err
	})
}

// ForgetRecords retains reports while clearing references to removed worker data.
func (d *Store) ForgetRecords(ids []string) error {
	return d.Transaction(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec("UPDATE agent_sessions SET snapshot_id=CASE WHEN snapshot_id=? THEN NULL ELSE snapshot_id END,active_job_id=CASE WHEN active_job_id=? THEN NULL ELSE active_job_id END WHERE node_id=? AND (snapshot_id=? OR active_job_id=?)", id, id, d.NodeID, id, id); err != nil {
				return err
			}
		}
		return nil
	})
}

//go:embed schema.sql
var schema string

func Initialize(tx *sql.Tx) error {
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO agent_settings(id,value) VALUES(1,?)", httpapi.JSONText(defaultConfig()))
	return err
}
