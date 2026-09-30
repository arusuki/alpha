package bastion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/tailscale"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

type networkFake struct {
	created, deleted int
	createErr        error
	invites          []tailscale.Invite
}

func (f *networkFake) Dispatch(http.ResponseWriter, *http.Request, platform.User) (int, any, error) {
	return 200, nil, nil
}
func (f *networkFake) Devices(context.Context) ([]tailscale.Device, error) { return nil, nil }
func (f *networkFake) Invites(context.Context, string) ([]tailscale.Invite, error) {
	if f.invites == nil {
		return []tailscale.Invite{}, nil
	}
	return f.invites, nil
}
func (f *networkFake) CreateInvite(context.Context, string) (tailscale.Invite, error) {
	f.created++
	v := tailscale.Invite{ID: "invite-a", URL: "https://login.tailscale.com/admin/invite/one"}
	f.invites = append(f.invites, v)
	return v, f.createErr
}
func (f *networkFake) DeleteInvite(context.Context, string) error { f.deleted++; return nil }
func fixture(t *testing.T) (*Handler, *networkFake, members.Member) {
	t.Helper()
	db, e := platform.OpenDatabase(t.TempDir(), func(tx *sql.Tx) error {
		if e := members.Initialize(tx); e != nil {
			return e
		}
		return Initialize(tx)
	})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.SQL.Close() })
	h := NewHandler(db)
	f := &networkFake{}
	h.Tailscale = f
	h.KeyEditor = func(string, string) error { return nil }
	h.installation = func() error { return nil }
	if _, e = db.SQL.Exec("INSERT INTO bastion_tailscale VALUES('node-a','Node A',1)"); e != nil {
		t.Fatal(e)
	}
	store := &members.Store{Database: db}
	invite, e := store.CreateInvitation("test", 10, "test")
	if e != nil {
		t.Fatal(e)
	}
	m, e := store.RegisterWith(members.Registration{Username: "alice", SSHKey: testKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, h.Reserve)
	if e != nil {
		t.Fatal(e)
	}
	return h, f, m
}
func TestAccessIdempotencyAndCompleteRevocation(t *testing.T) {
	h, f, m := fixture(t)
	calls := []string{}
	h.KeyEditor = func(id, key string) error { calls = append(calls, id+key); return nil }
	for range 2 {
		if e := h.Apply(context.Background(), m.ID, testKey, false); e != nil {
			t.Fatal(e)
		}
	}
	if f.created != 1 || len(calls) != 1 {
		t.Fatalf("duplicate assignment %d %d", f.created, len(calls))
	}
	a, e := h.Access(m.ID)
	if e != nil || a.InviteState != "invited" || a.KeyState != "ready" {
		t.Fatalf("%+v %v", a, e)
	}
	f.invites[0].Accepted = true
	f.invites[0].AcceptedBy.LoginName = "user@example.test"
	if e = h.Refresh(context.Background(), m.ID, "", false, "operator"); e != nil {
		t.Fatal(e)
	}
	a, _ = h.Access(m.ID)
	if a.AcceptedBy != "user@example.test" {
		t.Fatal(a)
	}
	for range 2 {
		if e = h.Apply(context.Background(), m.ID, "", true); e != nil {
			t.Fatal(e)
		}
	}
	if f.deleted != 1 || len(calls) != 2 {
		t.Fatal("cleanup not idempotent")
	}
}
func TestUnknownInviteNeverRecreatedOrForgotten(t *testing.T) {
	h, f, m := fixture(t)
	f.createErr = errors.New("connection lost after creation")
	if e := h.Apply(context.Background(), m.ID, testKey, false); e == nil {
		t.Fatal("missing failure")
	}
	if e := h.Apply(context.Background(), m.ID, testKey, false); e != nil {
		t.Fatal(e)
	}
	if f.created != 1 {
		t.Fatal("recreated uncertain invite")
	}
	if e := h.Apply(context.Background(), m.ID, "", true); e == nil {
		t.Fatal("forgot uncertain invite")
	}
	if e := h.Refresh(context.Background(), m.ID, "invite-a", false, "operator"); e != nil {
		t.Fatal(e)
	}
	if e := h.Apply(context.Background(), m.ID, "", true); e != nil {
		t.Fatal(e)
	}
	if f.deleted != 1 {
		t.Fatal("did not revoke reconciled invite")
	}
}
func TestKeyFailureRetainsShareAndRetriesOnlyKey(t *testing.T) {
	h, f, m := fixture(t)
	h.KeyEditor = func(string, string) error { return errors.New("permission denied") }
	if e := h.Apply(context.Background(), m.ID, testKey, false); e == nil {
		t.Fatal("missing failure")
	}
	h.KeyEditor = func(string, string) error { return nil }
	h.installation = func() error { return nil }
	if e := h.Apply(context.Background(), m.ID, testKey, false); e != nil {
		t.Fatal(e)
	}
	if f.created != 1 {
		t.Fatal("duplicate invite")
	}
}

func TestConfirmAbsentRequiresNoNewInvites(t *testing.T) {
	h, f, m := fixture(t)
	f.createErr = errors.New("lost response")
	_ = h.Apply(context.Background(), m.ID, testKey, false)
	if e := h.Refresh(context.Background(), m.ID, "", true, "operator"); e == nil {
		t.Fatal("forgot a new invite")
	}
	f.invites = []tailscale.Invite{}
	if e := h.Refresh(context.Background(), m.ID, "", true, "operator"); e != nil {
		t.Fatal(e)
	}
	a, _ := h.Access(m.ID)
	if a.InviteState != "pending" {
		t.Fatal(a)
	}
}
