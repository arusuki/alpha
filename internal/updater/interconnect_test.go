package updater

import (
	"testing"

	"project-alpha/internal/platform"
)

func dropLinkSchema(t *testing.T, db *platform.Database, role string) {
	t.Helper()
	var sql string
	switch role {
	case "control":
		sql = "DROP TRIGGER link_guard_node_ip; DROP TRIGGER link_guard_node_delete; DROP TABLE link_endpoints; DROP TABLE link_nodes; DROP TABLE link_connections; DROP TABLE link_settings"
	case "worker":
		sql = "DROP TRIGGER link_guard_owner; DROP TRIGGER link_guard_release; DROP TABLE link_claims; DROP TABLE link_operations; DROP TABLE link_local"
	default:
		return
	}
	if _, err := db.SQL.Exec(sql); err != nil {
		t.Fatal(err)
	}
}
func checkLinkSchema(t *testing.T, db *platform.Database, role string) {
	t.Helper()
	var tables, triggers int
	err := db.SQL.QueryRow("SELECT count(*) FILTER(WHERE type='table'),count(*) FILTER(WHERE type='trigger') FROM sqlite_master WHERE name LIKE 'link_%'").Scan(&tables, &triggers)
	want := map[string]int{"control": 4, "worker": 3, "registry": 0}[role]
	if err != nil || tables != want || want > 0 && triggers != 2 || want == 0 && triggers != 0 {
		t.Fatalf("interconnect schema for %s: %d tables %d triggers (%v)", role, tables, triggers, err)
	}
}
