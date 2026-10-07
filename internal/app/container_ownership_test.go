package app

import (
	"database/sql"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestOwnershipCannotBypassSingleContainerOrClaim(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	put := func(id, name, owner string) error {
		_, err := db.SQL.Exec("INSERT INTO managed_containers VALUES(?,'unix:///test.sock','daemon',?,?,'{}','fp','adopt','',1,0)", id, name, owner)
		return err
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err = put(a, "alice", "alice"); err != nil {
		t.Fatal(err)
	}
	if err = put(b, "spare", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("UPDATE owners SET owner='alice' WHERE container_id=?", b); err == nil {
		t.Fatal("overlay bypassed one-container limit")
	}
	if err = put(strings.Repeat("c", 64), "second", "alice"); err == nil {
		t.Fatal("managed import bypassed limit")
	}
	if _, err = db.SQL.Exec("UPDATE owners SET owner='bob' WHERE container_id=?", a); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err = db.SQL.QueryRow("SELECT owner FROM managed_containers WHERE id=?", a).Scan(&owner); err != nil || owner != "bob" {
		t.Fatalf("overlay was not synchronized: %s %v", owner, err)
	}
	id := strings.Repeat("1", 32)
	if _, err = db.SQL.Exec("INSERT INTO member_container_slots(member_id,username,plan,deleted,mode,container_id) VALUES(?,'bob','',0,'adopt',?)", id, a); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("UPDATE owners SET owner='carol' WHERE container_id=?", a); err == nil {
		t.Fatal("reassigned claimed container")
	}
	if err = db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE member_container_slots SET deleted=1 WHERE member_id=?", id); err != nil {
			return err
		}
		return newContainerHandler(db).UnassignOwner(tx, "bob")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("UPDATE owners SET owner='carol' WHERE container_id=?", a); err != nil {
		t.Fatal(err)
	}
}
