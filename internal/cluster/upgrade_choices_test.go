package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func TestControlUpgradeRetainsChoiceAndRegistrationRetry(t *testing.T) {
	dir := t.TempDir()
	db, err := platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	store := &members.Store{Database: db}
	invite, err := store.CreateInvitation("upgrade", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: resourceTestKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}
	m, err := store.RegisterWith(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	node := strings.Repeat("1", 32)
	target := strings.Repeat("a", 64)
	if _, err = db.SQL.Exec("INSERT INTO cluster_nodes VALUES(?,'worker','http://127.0.0.1:1',?,0,'worker','10.0.0.1')", node, strings.Repeat("t", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO member_node_resources(member_id,node_id,state,container_id,name,port,updated_at) VALUES(?,?,'ready',?,'existing',2222,123)", m.ID, node, target); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec(`ALTER TABLE members DROP COLUMN registration_containers;
 ALTER TABLE member_node_resources DROP COLUMN mode; ALTER TABLE member_node_resources DROP COLUMN target_id; PRAGMA user_version=33;`); err != nil {
		t.Fatal(err)
	}
	db.SQL.Close()
	db, err = platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	var state, cid, mode string
	if err = db.SQL.QueryRow("SELECT state,container_id,mode FROM member_node_resources WHERE member_id=?", m.ID).Scan(&state, &cid, &mode); err != nil || state != "ready" || cid != target || mode != "create" {
		t.Fatalf("lost allocation: %s %s %s %v", state, cid, mode, err)
	}
	req.Containers = []members.ContainerChoice{{NodeID: node, Mode: "create"}}
	// This uses the migrated choices, whose JSON field order can differ from a
	// fresh registration. Equality is semantic, not serialized object order.
	if _, err = members.NewHandler(db).RegisterRegistry(req, m.ResourceToken); err != nil {
		t.Fatalf("lost idempotent registration: %v", err)
	}
}
