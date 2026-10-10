package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed mihomo.sql
var mihomoSchema string

// New databases of every role include this schema.
func installMihomo(tx *sql.Tx) error {
	_, err := tx.Exec(mihomoSchema)
	return err
}
