package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed assistant.sql
var assistantSchema string

//go:embed assistant_settings.sql
var assistantSettingsSchema string

//go:embed member_assistant.sql
var memberAssistantSchema string

func InstallAssistant(tx *sql.Tx) error {
	if _, err := tx.Exec(assistantSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(assistantSettingsSchema); err != nil {
		return err
	}
	_, err := tx.Exec(memberAssistantSchema)
	return err
}

func upgradeMemberAssistant(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "control" {
		_, err := tx.Exec(memberAssistantSchema)
		return err
	}
	return nil
}

func upgradeAssistant(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "control" {
		_, err := tx.Exec(assistantSchema)
		return err
	}
	return nil
}

func upgradeAssistantSettings(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role == "control" {
		_, err := tx.Exec(assistantSettingsSchema)
		return err
	}
	return nil
}
