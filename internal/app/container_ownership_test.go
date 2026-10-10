package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
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

type unavailableServiceMounts struct{ platform.Module }

func (unavailableServiceMounts) ReconcileServices(context.Context) error {
	return errors.New("rootless unavailable")
}

func TestOwnershipResultSurvivesServiceMountFailure(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	id := strings.Repeat("a", 64)
	m := Modules{Storage: storage.NewHandler(storage.NewStore(db), nil), Containers: unavailableServiceMounts{}}
	r := httptest.NewRequest("PUT", "/api/owners", strings.NewReader(`{"container_id":"`+id+`","owner":"alice"}`))
	r.Header.Set("Content-Type", "application/json")
	status, value, err := m.Dispatch(httptest.NewRecorder(), r, platform.User{Username: "admin", Role: "admin"})
	if err != nil || status != 200 {
		t.Fatal("saved ownership reported as failed", status, err)
	}
	result := value.(map[string]any)
	if result["owners"].(map[string]string)[id] != "alice" || !strings.Contains(result["warning"].(string), "rootless unavailable") {
		t.Fatal("missing ownership or mount warning", result)
	}
	var owner string
	if err = db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id=?", id).Scan(&owner); err != nil || owner != "alice" {
		t.Fatal(owner, err)
	}
}

func TestMountedContainerOwnershipOverlayIsProtected(t *testing.T) {
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err = db.SQL.Exec(`INSERT INTO managed_containers VALUES('mounted','unix:///test.sock','daemon','training','alice','{}','fp','adopt','',1,0);
INSERT INTO node_service_mounts(container_id,endpoint,daemon,owner,name,socket_path) VALUES('mounted','unix:///test.sock','daemon','alice','training','/var/run/docker.sock')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("UPDATE owners SET owner='bob' WHERE container_id='mounted'"); err == nil {
		t.Fatal("ownership overlay bypassed mount revocation")
	}
	var owner string
	if err = db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id='mounted'").Scan(&owner); err != nil || owner != "alice" {
		t.Fatal("partial owner change", owner, err)
	}
	if _, err = db.SQL.Exec("DELETE FROM node_service_mounts; UPDATE owners SET owner='bob' WHERE container_id='mounted'"); err != nil {
		t.Fatal(err)
	}
	if err = db.SQL.QueryRow("SELECT owner FROM managed_containers WHERE id='mounted'").Scan(&owner); err != nil || owner != "bob" {
		t.Fatal(owner, err)
	}
}
