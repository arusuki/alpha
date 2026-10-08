package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed mihomo.sql
var mihomoSchema string

// The same schema is installed for every role and by the standalone updater.
func installMihomo(tx *sql.Tx) error {
	_, err := tx.Exec(mihomoSchema)
	return err
}
