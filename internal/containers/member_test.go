package containers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memberKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

func memberCall(h *Handler, method, id string) (int, string) {
	raw, _ := json.Marshal(map[string]string{"username": "bob", "ssh_public_key": memberKey})
	return call(h, method, "/api/containers/members/"+id, string(raw), admin)
}
func TestMemberRetryAfterCreateStartFailureAndDeleteData(t *testing.T) {
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
	for range 2 {
		if status, body := memberCall(h, "DELETE", id); status != 200 {
			t.Fatalf("delete %d %s", status, body)
		}
	}
	if _, e := os.Stat(filepath.Join(cfg.BaseDir, "alpha-"+id)); !os.IsNotExist(e) {
		t.Fatalf("member data remains: %v", e)
	}
	if raw, e := os.ReadFile(sentinel); e != nil || string(raw) != "keep" {
		t.Fatal("shared data removed")
	}
	if status, _ := memberCall(h, "PUT", id); status == 200 {
		t.Fatal("deleted identity recreated")
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
func TestMemberDeleteRetainsRecordsWhenDaemonUnavailable(t *testing.T) {
	h, f, _ := fixture(t)
	f.absent = true
	id := strings.Repeat("e", 32)
	if status, body := memberCall(h, "PUT", id); status != 200 {
		t.Fatalf("create %d %s", status, body)
	}
	f.failAction = "info"
	if status, _ := memberCall(h, "DELETE", id); status == 200 {
		t.Fatal("forgot offline resources")
	}
	records, e := h.records()
	if e != nil || len(records) != 1 {
		t.Fatal("record lost")
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
