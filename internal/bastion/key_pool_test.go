package bastion

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func poolFixture(t *testing.T) (*Handler, keyStore, installation, members.Member) {
	t.Helper()
	h, _, member := fixture(t)
	s, c := keyFixture(t)
	if _, err := h.DB.SQL.Exec("INSERT INTO service_identity VALUES(1,'control',?)", c.ControlID); err != nil {
		t.Fatal(err)
	}
	id, err := h.DB.CheckMode("control")
	if err != nil {
		t.Fatal(err)
	}
	c.ControlID = id
	raw, _ := json.Marshal(c)
	if err = os.WriteFile(filepath.Join(s.path, "installation.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	h.keys, h.KeyEditor = s, h.editMemberKey
	return h, s, c, member
}

func anotherPoolKey() string {
	parts := strings.Fields(testKey)
	blob, _ := base64.StdEncoding.DecodeString(parts[1])
	blob[len(blob)-1] ^= 1
	return parts[0] + " " + base64.StdEncoding.EncodeToString(blob)
}

func TestPoolSupersetAndContentIndex(t *testing.T) {
	h, s, c, member := poolFixture(t)
	foreign, extra := strings.Repeat("d", 32), strings.Repeat("e", 32)
	if err := setPoolEntry(s, c.ControlID, foreign, testKey); err != nil {
		t.Fatal(err)
	}
	if err := setPoolEntry(s, c.ControlID, extra, anotherPoolKey()); err != nil {
		t.Fatal(err)
	}
	if err := h.SyncKeys(); err != nil {
		t.Fatal(err)
	}
	entries, err := h.keyPool()
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	if entries[0].ID != foreign || entries[0].State != "used" || len(entries[0].Members) != 1 || entries[0].Members[0].ID != member.ID || entries[1].State != "free" {
		t.Fatal("pool entries were mistaken for member IDs", entries)
	}
	access, err := h.Access(member.ID)
	if err != nil || access.KeyState != "ready" {
		t.Fatal(access, err)
	}
	if err = h.cleanFreeKey(foreign, "admin"); err == nil {
		t.Fatal("cleaned a referenced key")
	}
	if err = h.cleanFreeKey(extra, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = setPoolEntry(s, c.ControlID, foreign, ""); err != nil {
		t.Fatal(err)
	}
	if err = h.SyncKeys(); err != nil {
		t.Fatal(err)
	}
	entries, err = h.keyPool()
	if err != nil || len(entries) != 1 || entries[0].PublicKey != testKey || entries[0].State != "used" {
		t.Fatal("missing member key was not added", entries, err)
	}
}

func TestPoolSharedKeyAndCleanupRechecksCurrentMembers(t *testing.T) {
	h, s, c, first := poolFixture(t)
	freeID := strings.Repeat("e", 32)
	if err := setPoolEntry(s, c.ControlID, freeID, anotherPoolKey()); err != nil {
		t.Fatal(err)
	}
	before, err := h.keyPool()
	if err != nil || before[0].State != "free" {
		t.Fatal(before, err)
	}
	store := &members.Store{Database: h.DB}
	invite, err := store.CreateInvitation("pool", 3, "test")
	if err != nil {
		t.Fatal(err)
	}
	register := func(name, key string) members.Member {
		m, err := store.RegisterWith(members.Registration{Username: name, SSHKey: key, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, h.Reserve)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	register("ownsfree", anotherPoolKey())
	if err = h.cleanFreeKey(freeID, "admin"); err == nil {
		t.Fatal("cleanup trusted stale free status")
	}
	second := register("shared", testKey)
	if err = h.SyncKeys(); err != nil {
		t.Fatal(err)
	}
	entries, err := h.keyPool()
	if err != nil || len(entries) != 2 {
		t.Fatal("shared key was duplicated", entries, err)
	}
	for _, m := range []members.Member{first, second} {
		if _, err = h.DB.SQL.Exec("UPDATE members SET status='deleting' WHERE id=?", m.ID); err != nil {
			t.Fatal(err)
		}
		if err = h.editMemberKey(m.ID, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = h.DB.SQL.Exec("UPDATE member_access SET key_state='deleted' WHERE member_id=?", m.ID); err != nil {
			t.Fatal(err)
		}
		v, err := s.snapshot(c.ControlID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, key := range v.Keys {
			found = found || key == testKey
		}
		if found != (m.ID == first.ID) {
			t.Fatal("shared key revocation did not follow current references")
		}
	}
}

func TestPoolAPIAdminAndFreeCleanup(t *testing.T) {
	h, s, c, _ := poolFixture(t)
	entry := strings.Repeat("f", 32)
	if err := setPoolEntry(s, c.ControlID, entry, anotherPoolKey()); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"viewer", "admin"} {
		r := httptest.NewRequest("DELETE", "/api/bastion/keys/"+entry, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		_, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Role: role, Username: role})
		if (err == nil) != (role == "admin") {
			t.Fatalf("%s: %v", role, err)
		}
	}
}

func TestSyncPreservesUnrelatedEntryAndDoesNotReauthorizeDeletingUser(t *testing.T) {
	h, s, c, member := poolFixture(t)
	if err := setPoolEntry(s, c.ControlID, member.ID, anotherPoolKey()); err != nil {
		t.Fatal(err)
	}
	if err := h.SyncKeys(); err != nil {
		t.Fatal(err)
	}
	v, err := s.snapshot(c.ControlID)
	if err != nil || len(v.Keys) != 2 || v.Keys[member.ID] != anotherPoolKey() {
		t.Fatal("member ID overwrote an unrelated pool entry", v, err)
	}
	if _, err = h.DB.SQL.Exec("UPDATE members SET status='deleting' WHERE id=?", member.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.editMemberKey(member.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Model the gap before the worker commits key_state='deleted'.
	if err = h.SyncKeys(); err != nil {
		t.Fatal(err)
	}
	v, err = s.snapshot(c.ControlID)
	if err != nil || len(v.Keys) != 1 || v.Keys[member.ID] != anotherPoolKey() {
		t.Fatal("synchronization restored a revoked key", v, err)
	}
}

func TestAdoptionAllowsDepartedServiceButNotChangedJump(t *testing.T) {
	s, c := keyFixture(t)
	lookup := s.lookup
	s.lookup = func(name string) (*user.User, error) {
		if name == c.ServiceUser {
			return nil, user.UnknownUserError(name)
		}
		return lookup(name)
	}
	if _, _, err := s.open(); err == nil {
		t.Fatal("normal publication accepted missing service identity")
	}
	r, _, err := s.openForAdoption()
	if err != nil {
		t.Fatal("adoption depended on departed service", err)
	}
	r.Close()
	s.lookup = func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) }
	if _, _, err = s.openForAdoption(); err == nil {
		t.Fatal("adoption accepted a missing jump account")
	}
	var output bytes.Buffer
	if err = s.authorizedKeys(JumpUser, "1", &output); err == nil {
		t.Fatal("ordinary reader skipped identity checks")
	}
}
