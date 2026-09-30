package app

import (
	"encoding/json"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestNodeInventoryMergesManagedAndScannedOwnership(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err = db.CheckMode("control"); err == nil {
		t.Fatal("worker directory accepted as control")
	}
	if _, err = db.SQL.Exec("SELECT * FROM members"); err == nil {
		t.Fatal("worker initialized central members")
	}
	if configured, err := db.Configured(); err != nil || configured {
		t.Fatal("worker should have no local account")
	}
	initial, err := inventory(db)
	if err != nil || len(initial.Containers) != 0 {
		t.Fatalf("empty inventory: %+v %v", initial, err)
	}
	id := strings.Repeat("a", 32)
	metadata := `{"finished_at":"2026-09-30T10:00:00Z","containers":[{"id":"one","name":"scan-name","owner":"label","state":"running"},{"id":"two","name":"scan-only","owner":"bob","state":"exited"}]}`
	if _, err = db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','admin',1,2,'{}')", id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO snapshot_records VALUES(?,1,'/srv',?)", id, metadata); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, name, owner string }{{"one", "managed-name", "managed-owner"}, {"three", "new-container", "alice"}} {
		if _, err = db.SQL.Exec("INSERT INTO managed_containers VALUES(?,'unix:///test.sock','daemon',?,?, '{}','fingerprint','create','',1,3)", c.id, c.name, c.owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.SQL.Exec("INSERT INTO owners VALUES('one','alice')"); err != nil {
		t.Fatal(err)
	}
	inv, err := inventory(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Containers) != 3 || inv.SnapshotID != id {
		t.Fatalf("wrong inventory: %+v", inv)
	}
	for _, c := range inv.Containers {
		if c.ID == "one" && (c.Name != "managed-name" || c.Owner != "alice" || !c.Managed || c.ObservedAt == "" || c.State != "running") {
			t.Fatalf("bad merge: %+v", c)
		}
		if c.ID == "three" && (c.ObservedAt != "" || c.State != "" || !c.Managed) {
			t.Fatalf("invented snapshot observation: %+v", c)
		}
	}
	raw, _ := json.Marshal(inv)
	if strings.Contains(string(raw), "fingerprint") || strings.Contains(string(raw), "unix:///test.sock") {
		t.Fatal("inventory leaked internal management metadata")
	}
}
