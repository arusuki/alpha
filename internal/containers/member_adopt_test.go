package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"project-alpha/internal/platform"
	"strings"
	"testing"
)

func adoptionCall(h *Handler, id, username, target string) (int, string) {
	records, _ := h.records()
	owner := ""
	for _, r := range records {
		if r.ID == target {
			owner = r.Owner
		}
	}
	raw, _ := json.Marshal(map[string]string{"expected_owner": owner, "username": username, "ssh_public_key": memberKey, "password": "Member-password-123", "mode": "adopt", "container_id": target})
	return call(h, "PUT", "/api/containers/members/"+id, string(raw), admin)
}
func TestMemberAdoptionPreservesContainerAndRetriesKeyInstall(t *testing.T) {
	h, f, _ := fixture(t)
	adopt(t, h)
	original := fingerprint(f.c)
	id := strings.Repeat("1", 32)
	f.calls = nil
	f.inputs = nil
	f.failAction = "exec"
	if status, _ := adoptionCall(h, id, "bob", f.c.ID); status == 200 {
		t.Fatal("key installation should fail")
	}
	var target string
	if err := h.db.SQL.QueryRow("SELECT container_id FROM member_container_slots WHERE member_id=?", id).Scan(&target); err != nil || target != f.c.ID {
		t.Fatalf("lost claim: %s %v", target, err)
	}
	// Even while SSH installation is unfinished, a second user cannot take it.
	if status, body := adoptionCall(h, strings.Repeat("2", 32), "carol", f.c.ID); status == 200 || !strings.Contains(body, "已领养") {
		t.Fatalf("second claim: %d %s", status, body)
	}
	f.failAction = ""
	for range 2 {
		if status, body := adoptionCall(h, id, "bob", f.c.ID); status != 200 {
			t.Fatalf("retry: %d %s", status, body)
		}
	}
	if fingerprint(f.c) != original {
		t.Fatal("adoption changed configuration")
	}
	for i, args := range f.calls {
		if args[0] != "info" && args[0] != "container" && args[0] != "exec" && args[0] != "ps" {
			t.Fatalf("adoption changed lifecycle: %v", args)
		}
		if strings.Contains(strings.Join(args, " "), "chpasswd") || strings.Contains(f.inputs[i], "Member-password-123") {
			t.Fatal("adoption reset password")
		}
	}
	records, _ := h.records()
	if len(records) != 1 || records[0].Owner != "bob" {
		t.Fatalf("wrong ownership: %+v", records)
	}
	// Recorded identity must not follow a name reused by another container.
	f.c.Config.Image = "changed-image"
	if status, _ := adoptionCall(h, id, "bob", f.c.ID); status == 200 {
		t.Fatal("accepted changed identity")
	}
}
func TestAdoptionRejectsSecondContainerForOwner(t *testing.T) {
	h, f, _ := fixture(t)
	adopt(t, h)
	records, _ := h.records()
	existing := records[0]
	existing.ID = strings.Repeat("d", 64)
	existing.Name = "another"
	existing.Owner = "bob"
	if err := h.save(existing, "admin"); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if status, _ := adoptionCall(h, strings.Repeat("1", 32), "bob", f.c.ID); status == 200 {
		t.Fatal("accepted second container")
	}
	if len(f.calls) != before {
		t.Fatal("mutated Docker before checking existing owner")
	}
}

func TestCandidatesListOnlyManagedContainersWithoutDocker(t *testing.T) {
	h, f, _ := fixture(t)
	id := strings.Repeat("3", 32)
	candidates, err := h.candidates()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(candidates)
	if strings.Contains(string(raw), f.c.ID) {
		t.Fatal("unmanaged Docker container offered for adoption")
	}
	adopt(t, h)
	if status, out := adoptionCall(h, id, "bob", f.c.ID); status != 200 {
		t.Fatalf("adoption: %d %s", status, out)
	}
	// Once managed, the container remains listed with its claim even if Docker
	// cannot be queried; unrelated live containers are never enumerated.
	f.failAction = "ps"
	f.calls = nil
	status, listed := call(h, "GET", "/api/containers/candidates", "", admin)
	var result struct {
		Containers []candidate `json:"containers"`
	}
	if err := json.Unmarshal([]byte(listed), &result); err != nil {
		t.Fatal(err)
	}
	if status != 200 || len(result.Containers) != 1 || result.Containers[0].ID != f.c.ID || result.Containers[0].MemberID != id || result.Containers[0].ClaimedBy != "bob" {
		t.Fatalf("managed claim missing: %d %s", status, listed)
	}
	if len(f.calls) != 0 {
		t.Fatalf("candidate list queried Docker: %v", f.calls)
	}
}
func TestUnmanagedAdoptionRejectsWithoutReservation(t *testing.T) {
	h, f, _ := fixture(t)
	id := strings.Repeat("4", 32)
	if status, out := adoptionCall(h, id, "bob", f.c.ID); status == 200 || !strings.Contains(out, "请先通过 CLI 导入") {
		t.Fatalf("unmanaged adoption: %d %s", status, out)
	}
	var slots int
	if err := h.db.SQL.QueryRow("SELECT count(*) FROM member_container_slots").Scan(&slots); err != nil || slots != 0 {
		t.Fatalf("rejected adoption left a reservation: %d %v", slots, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("unmanaged adoption called Docker: %v", f.calls)
	}
	// After an explicit import, the same member can claim the container.
	adopt(t, h)
	if status, out := adoptionCall(h, id, "bob", f.c.ID); status != 200 {
		t.Fatalf("adoption after import: %d %s", status, out)
	}
}

func TestConcurrentAdoptionAcrossHandlersHasOneWinner(t *testing.T) {
	h, f, _ := fixture(t)
	adopt(t, h)
	other := NewHandler(h.db)
	other.run = f.run
	target := f.c.ID
	type result struct {
		status int
		body   string
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i, handler := range []*Handler{h, other} {
		go func(i int, handler *Handler) {
			<-start
			status, body := adoptionCall(handler, strings.Repeat(string(rune('5'+i)), 32), []string{"bob", "carol"}[i], target)
			results <- result{status, body}
		}(i, handler)
	}
	close(start)
	successes := 0
	for range 2 {
		r := <-results
		if r.status == 200 {
			successes++
		} else if !strings.Contains(r.body, "已领养") {
			t.Fatalf("unexpected conflict: %+v", r)
		}
	}
	if successes != 1 {
		t.Fatalf("adoption winners: %d", successes)
	}
	var slots int
	h.db.SQL.QueryRow("SELECT count(*) FROM member_container_slots WHERE deleted=0").Scan(&slots)
	if slots != 1 {
		t.Fatalf("conflicting claims persisted: %d", slots)
	}
}

func TestDeleteFreesOwnershipButReleaseRetainsIt(t *testing.T) {
	for _, action := range []string{"delete", "release"} {
		t.Run(action, func(t *testing.T) {
			h, f, _ := fixture(t)
			if _, err := h.db.SQL.Exec("CREATE TABLE owners(container_id TEXT PRIMARY KEY,owner TEXT NOT NULL); CREATE UNIQUE INDEX one_owner ON owners(owner) WHERE owner<>''"); err != nil {
				t.Fatal(err)
			}
			if err := h.db.Transaction(platform.InstallContainerOwnership); err != nil {
				t.Fatal(err)
			}
			h.Owner = func(tx *sql.Tx, id, owner string) error {
				_, err := tx.Exec("INSERT INTO owners VALUES(?,?) ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner", id, owner)
				return err
			}
			adopt(t, h)
			id := strings.Repeat("6", 32)
			if status, body := adoptionCall(h, id, "bob", f.c.ID); status != 200 {
				t.Fatalf("adopt: %d %s", status, body)
			}
			records, _ := h.records()
			if err := h.removeRecord(records[0], "admin", action); err != nil {
				t.Fatal(err)
			}
			var owner, cid string
			if err := h.db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id=?", f.c.ID).Scan(&owner); err != nil {
				t.Fatal(err)
			}
			if err := h.db.SQL.QueryRow("SELECT container_id FROM member_container_slots WHERE member_id=?", id).Scan(&cid); err != nil {
				t.Fatal(err)
			}
			if action == "delete" && (owner != "" || cid != "") {
				t.Fatalf("delete retained ownership: %s %s", owner, cid)
			}
			if action == "release" && (owner != "bob" || cid != f.c.ID) {
				t.Fatalf("release forgot existing container: %s %s", owner, cid)
			}
		})
	}
}

func TestCreateRejectsOwnerOfUnimportedLiveContainer(t *testing.T) {
	h, f, cfg := fixture(t)
	_, err := h.create(context.Background(), cfg, CreateRequest{Name: "new-alice", Owner: "alice"}, "admin")
	if err == nil || !strings.Contains(err.Error(), "已有容器") {
		t.Fatalf("created a second unscanned container: %v", err)
	}
	for _, args := range f.calls {
		if args[0] == "create" {
			t.Fatal("created before checking live ownership")
		}
	}
}
