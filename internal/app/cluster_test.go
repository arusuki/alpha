package app

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
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
	if err != nil || len(initial.Containers) != 0 || len(initial.Filesystems) != 0 || initial.SnapshotID != "" {
		t.Fatalf("empty inventory: %+v %v", initial, err)
	}
	id := strings.Repeat("a", 32)
	metadata := `{"finished_at":"2026-09-30T10:00:00Z","filesystems":[{"mount":"/","fs":"ext4","total":1000,"used":750},{"mount":"/data","fs":"xfs","total":2000,"used":0},{"mount":"/unknown","fs":"xfs","total":null,"used":null}],"containers":[{"id":"one","name":"scan-name","owner":"label","state":"running"},{"id":"two","name":"scan-only","owner":"bob","state":"exited"}]}`
	if _, err = db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','admin',1,2,'{}')", id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO snapshot_records VALUES(?,1,'/srv',?)", id, metadata); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, name, owner string }{{"one", "managed-name", "managed-owner"}, {"three", "new-container", "charlie"}} {
		if _, err = db.SQL.Exec("INSERT INTO managed_containers VALUES(?,'unix:///test.sock','daemon',?,?, '{}','fingerprint','create','',1,3)", c.id, c.name, c.owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.SQL.Exec("INSERT INTO owners VALUES('one','alice') ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner"); err != nil {
		t.Fatal(err)
	}
	inv, err := inventory(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Containers) != 3 || inv.SnapshotID != id {
		t.Fatalf("wrong inventory: %+v", inv)
	}
	if len(inv.Filesystems) != 3 || inv.Filesystems[0].Mount != "/" || inv.Filesystems[0].FS != "ext4" || *inv.Filesystems[0].Used != 750 || *inv.Filesystems[0].Total != 1000 || *inv.Filesystems[1].Used != 0 || inv.Filesystems[2].Used != nil {
		t.Fatalf("wrong disk capacities: %+v", inv.Filesystems)
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
	// Unreachable Docker must prevent deletion before keys can be revoked.
	// The ownership callback then clears managed and scanned ownership together.
	h := newContainerHandler(db)
	r := httptest.NewRequest("DELETE", "/api/containers/members/"+strings.Repeat("b", 32), strings.NewReader(`{"username":"alice"}`))
	r.Header.Set("Content-Type", "application/json")
	status, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Username: "admin", Role: "admin"})
	if err == nil || status == 200 {
		t.Fatalf("deleted without confirming key revocation: %d %v", status, err)
	}
	var owner string
	if err = db.SQL.QueryRow("SELECT owner FROM managed_containers WHERE id='one'").Scan(&owner); err != nil || owner != "alice" {
		t.Fatalf("released ownership before key revocation: %s %v", owner, err)
	}
	// Exercise the callback used after successful revocation; Docker/key writes
	// and deletion retries are covered by the containers package's member tests.
	if err = db.Transaction(func(tx *sql.Tx) error { return h.UnassignOwner(tx, "alice") }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		inv, err = inventory(db)
		if err != nil || len(inv.Containers) != 3 {
			t.Fatalf("retained inventory: %+v %v", inv, err)
		}
		for _, c := range inv.Containers {
			if c.ID == "two" && c.Owner != "bob" || c.ID == "one" && c.Owner != "" || c.ID == "three" && c.Owner != "charlie" {
				t.Fatalf("wrong retained ownership: %+v", c)
			}
		}
		// A later scan still contains the immutable original owner labels.
		if _, err = db.SQL.Exec("UPDATE snapshot_records SET metadata=?", `{"containers":[{"id":"one","owner":"alice"},{"id":"two","owner":"bob"},{"id":"three","owner":"alice"}]}`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnassignPreservesOtherOwnerOverridesAndHistoricalContainers(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	job := strings.Repeat("a", 32)
	if _, err = db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,'completed','manual','admin',1,2,'{}')", job); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO snapshot_records VALUES(?,1,'/srv',?)", job, `{"containers":[{"id":"scan-only","owner":"alice"},{"id":"reassigned","owner":"alice"}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO managed_containers VALUES('reassigned','unix:///test.sock','daemon','reassigned','alice','{}','fingerprint','create','',1,3)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO owners VALUES('reassigned','bob') ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner"); err != nil {
		t.Fatal(err)
	}
	h := newContainerHandler(db)
	if err = db.Transaction(func(tx *sql.Tx) error { return h.UnassignOwner(tx, "alice") }); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err = db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id='scan-only'").Scan(&owner); err != nil || owner != "" {
		t.Fatalf("scan ownership %q %v", owner, err)
	}
	if err = db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id='reassigned'").Scan(&owner); err != nil || owner != "bob" {
		t.Fatalf("other ownership %q %v", owner, err)
	}
}
