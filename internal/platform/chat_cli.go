package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed chat_cli_control.sql
var chatCLIControlSchema string

//go:embed chat_cli_worker.sql
var chatCLIWorkerSchema string

func InstallControlChatCLI(tx *sql.Tx) error { _, err := tx.Exec(chatCLIControlSchema); return err }
func InstallWorkerChatCLI(tx *sql.Tx) error  { _, err := tx.Exec(chatCLIWorkerSchema); return err }

func upgradeChatCLI(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	switch role {
	case "control":
		return InstallControlChatCLI(tx)
	case "worker":
		return InstallWorkerChatCLI(tx)
	}
	return nil
}
