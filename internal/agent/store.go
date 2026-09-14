package agent

import (
	"database/sql"
	_ "embed"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type object = map[string]any

// Store owns model settings and analysis conversations.
type Store struct{ *platform.Database }

func NewStore(db *platform.Database) *Store { return &Store{db} }

//go:embed schema.sql
var schema string

func Initialize(tx *sql.Tx) error {
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO agent_settings(id,value) VALUES(1,?)", httpapi.JSONText(defaultConfig()))
	return err
}
