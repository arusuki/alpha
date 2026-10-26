package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed member_node_permissions.sql
var memberNodePermissionsSchema string

func InstallMemberNodePermissions(tx *sql.Tx) error {
	_, err := tx.Exec(memberNodePermissionsSchema)
	return err
}

func upgradeMemberNodePermissions(tx *sql.Tx) error {
	var role string
	if err := tx.QueryRow("SELECT mode FROM service_identity WHERE id=1").Scan(&role); err != nil {
		return err
	}
	if role != "control" {
		return nil
	}
	if err := InstallMemberNodePermissions(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO member_node_permissions SELECT id,'{"all_nodes":true,"node_ids":[]}' FROM members;
INSERT INTO invitation_node_permissions SELECT id,'{"all_nodes":true,"node_ids":[]}' FROM member_invitations;`)
	return err
}
