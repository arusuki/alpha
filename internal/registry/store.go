// Package registry provides a public registration gateway to an outbound-connected control.
package registry

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"

	"project-alpha/internal/platform"
)

//go:embed schema.sql
var schemaSQL string

func Initialize(tx *sql.Tx) error {
	if _, err := tx.Exec(schemaSQL); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO service_identity VALUES(1,'registry',?)", platform.RandomHex(16))
	return err
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

// Binding is permanent for this data directory, including across process restarts.
func bindControl(db *platform.Database, id string) error {
	return db.Transaction(func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRow("SELECT control_id FROM registry_control WHERE id=1").Scan(&current)
		if err == sql.ErrNoRows {
			_, err = tx.Exec("INSERT INTO registry_control VALUES(1,?)", id)
			return err
		}
		if err != nil {
			return err
		}
		if current != id {
			return fmt.Errorf("registry is bound to another control; use a new data directory")
		}
		return nil
	})
}
