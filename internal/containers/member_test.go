package containers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memberKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

func memberCall(h *Handler, method, id string) (int, string) {
	raw, _ := json.Marshal(map[string]string{"mode": "create", "username": "bob", "ssh_public_key": memberKey, "password": "Member-password-123"})
	return call(h, method, "/api/containers/members/"+id, string(raw), admin)
}

func TestMemberUnassignmentRollsBackOnOwnershipFailure(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	id := strings.Repeat("1", 32)
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("create %d %s", status, body)
	}
	h.UnassignOwner = func(*sql.Tx, string) error { return errors.New("ownership unavailable") }
	if status, _ := memberCall(h, "DELETE", id); status == 200 {
		t.Fatal("forgot ownership failure")
	}
	records, err := h.records()
	if err != nil || len(records) != 1 || records[0].Owner != "bob" {
		t.Fatalf("changed ownership: %+v %v", records, err)
	}
	var deleted bool
	if err = h.db.SQL.QueryRow("SELECT deleted FROM member_container_slots WHERE member_id=?", id).Scan(&deleted); err != nil || deleted {
		t.Fatalf("lost retry slot: %v %v", deleted, err)
	}
}
func TestMemberRetryAfterCreateStartFailureAndUnassign(t *testing.T) {
	h, f, cfg := fixture(t)
	f.absent = true
	f.failAction = "start"
	id := strings.Repeat("b", 32)
	if status, _ := memberCall(h, "PUT", id); status == 200 {
		t.Fatal("expected start failure")
	}
	f.failAction = ""
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("retry %d %s", status, body)
	}
	if status, body := memberCall(h, "PUT", id); status != 200 || strings.Contains(body, "password") {
		t.Fatalf("repeat %d %s", status, body)
	}
	var plan string
	if err := h.db.SQL.QueryRow("SELECT plan FROM member_container_slots WHERE member_id=?", id).Scan(&plan); err != nil || strings.Contains(plan, "Member-password-123") {
		t.Fatalf("plan leaked password: %s %v", plan, err)
	}
	passwordWrites := 0
	for _, input := range f.inputs {
		if input == "root:Member-password-123\n" {
			passwordWrites++
		}
	}
	if passwordWrites != 1 {
		t.Fatalf("retry/reset password writes: %d", passwordWrites)
	}
	creates := 0
	keyWrites := 0
	for i, c := range f.calls {
		if c[0] == "create" {
			creates++
		}
		if strings.Contains(f.inputs[i], memberKey) {
			keyWrites++
		}
	}
	if creates != 1 || keyWrites != 1 {
		t.Fatalf("creates=%d keys=%d", creates, keyWrites)
	}
	sentinel := filepath.Join(cfg.BaseDir, "data", "shared")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	callsBeforeDelete := len(f.calls)
	for range 2 {
		if status, body := memberCall(h, "DELETE", id); status != 200 {
			t.Fatalf("delete %d %s", status, body)
		}
	}
	if len(f.calls) <= callsBeforeDelete || f.absent || !f.c.State.Running {
		t.Fatal("deletion must revoke keys and keep the container running")
	}
	records, err := h.records()
	if err != nil || len(records) != 1 || records[0].Owner != "" {
		t.Fatalf("container management/ownership: %+v %v", records, err)
	}
	if _, e := os.Stat(filepath.Join(cfg.BaseDir, "alpha-"+id)); e != nil {
		t.Fatalf("member data was removed: %v", e)
	}
	if raw, e := os.ReadFile(sentinel); e != nil || string(raw) != "keep" {
		t.Fatal("shared data removed")
	}
	if status, _ := memberCall(h, "PUT", id); status == 200 {
		t.Fatal("deleted identity recreated")
	}
	// A delayed repeat of the old deletion cannot clear a later assignment,
	// including a new member that reuses the same username.
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err != nil {
		t.Fatal(err)
	}
	if status, body := memberCall(h, "DELETE", id); status != 200 {
		t.Fatalf("repeat %d %s", status, body)
	}
	records, err = h.records()
	if err != nil || records[0].Owner != "bob" {
		t.Fatalf("repeat changed later ownership: %+v %v", records, err)
	}
}
func TestMemberCreateFailureReusesOnlyMarkedDirectory(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	f.failAction = "create"
	id := strings.Repeat("c", 32)
	if status, _ := memberCall(h, "PUT", id); status == 200 {
		t.Fatal("expected create failure")
	}
	f.failAction = ""
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("retry %d %s", status, body)
	}
}
func TestMemberRecoversDockerSuccessBeforeDatabaseWrite(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	id := strings.Repeat("d", 32)
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("create %d %s", status, body)
	}
	if _, e := h.db.SQL.Exec("DELETE FROM managed_containers"); e != nil {
		t.Fatal(e)
	}
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("recover %d %s", status, body)
	}
	creates := 0
	for _, c := range f.calls {
		if c[0] == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal("duplicate create")
	}
}
func TestMemberDeletePreservesOwnershipWhenDaemonUnavailable(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	id := strings.Repeat("e", 32)
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("create %d %s", status, body)
	}
	f.failAction = "info"
	if status, _ := memberCall(h, "DELETE", id); status == 200 {
		t.Fatal("deletion succeeded without revoking keys")
	}
	records, e := h.records()
	if e != nil || len(records) != 1 || records[0].Owner != "bob" {
		t.Fatal("record/ownership lost")
	}
	f.failAction = ""
	if status, body := memberCall(h, "DELETE", id); status != 200 {
		t.Fatalf("cleanup retry %d %s", status, body)
	}
}

func TestMemberCannotClaimOtherManagedContainer(t *testing.T) {
	h, f, _ := fixture(t)
	id := strings.Repeat("f", 32)
	f.c.Name = "/alpha-" + id
	// A pre-existing record with a guessed allocation name is not an allocation.
	r := Record{ID: f.c.ID, Endpoint: "unix:///var/run/docker.sock", Daemon: f.daemonID, Name: "alpha-" + id, Owner: "someone-else", Fingerprint: fingerprint(f.c), Origin: "create"}
	if e := h.save(r, "admin"); e != nil {
		t.Fatal(e)
	}
	if status, _ := memberCall(h, "PUT", id); status == 200 {
		t.Fatal("claimed another member's container")
	}
	for _, c := range f.calls {
		if c[0] == "exec" || c[0] == "start" || c[0] == "rm" {
			t.Fatalf("mutated unrelated container: %v", c)
		}
	}
}

func TestMemberKeyPatchAndDelete(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	id := strings.Repeat("9", 32)
	if code, body := memberCall(h, "PUT", id); code != 200 {
		t.Fatal(code, body)
	}
	// Existing public fixture; never generate test keys.
	other := "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBIvR3cOir2XFsX4NiA4QO1JKQ7c87emaiV0rBXS3fiseEt0seHFTvuv2Tl0Zz5jQJS1Ko0oVLFAQZ4BtLtn6hKg="
	patch := func(keys string) (int, string) {
		raw, _ := json.Marshal(map[string]string{"username": "bob", "ssh_public_key": keys})
		return call(h, "PATCH", "/api/containers/members/"+id, string(raw), admin)
	}
	before := len(f.inputs)
	if code, body := patch(memberKey + "\n" + other); code != 200 {
		t.Fatal(code, body)
	}
	if !strings.Contains(f.inputs[len(f.inputs)-1], memberKey) || !strings.Contains(f.inputs[len(f.inputs)-1], other) {
		t.Fatal("keys not delivered", f.inputs[before:])
	}
	if code, body := patch(other); code != 200 {
		t.Fatal(code, body)
	}
	if strings.Contains(f.inputs[len(f.inputs)-1], memberKey) {
		t.Fatal("old key delivered")
	}
	f.failAction = "exec"
	if code, _ := memberCall(h, "DELETE", id); code == 200 {
		t.Fatal("ignored key removal failure")
	}
	records, _ := h.records()
	if records[0].Owner != "bob" {
		t.Fatal("released ownership before revocation")
	}
	f.failAction = ""
	if code, body := memberCall(h, "DELETE", id); code != 200 {
		t.Fatal(code, body)
	}
	if f.inputs[len(f.inputs)-1] != "" {
		t.Fatal("deletion installed a key")
	}
	if code, _ := patch(other); code == 200 {
		t.Fatal("accepted patch after deletion")
	}
}
