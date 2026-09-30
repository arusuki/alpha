package bastion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/tailscale"
)

type deviceNetwork struct {
	*networkFake
	devices []tailscale.Device
}

func (n deviceNetwork) Devices(context.Context) ([]tailscale.Device, error) { return n.devices, nil }
func TestShareAdmissionRequiresSSHProtocolAndOwnedDevice(t *testing.T) {
	h, f, _ := fixture(t)
	h.Tailscale = deviceNetwork{f, []tailscale.Device{{NodeID: "new-share", Hostname: "Share", Authorized: true, Addresses: []string{"100.64.0.3"}}}}
	calls := 0
	fail := true
	h.RemoteCommand = func(ctx context.Context, s ShareNode, req commandRequest) (commandReply, error) {
		calls++
		if s.SSHHost != "100.64.0.3" || s.SSHPort != 2222 || req.Operation != "inspect" {
			t.Errorf("bad admission: %+v %+v", s, req)
		}
		if fail {
			return commandReply{}, httpapi.NewError(502, "SSH 认证失败")
		}
		return commandReply{Version: keyFormat, ListenHost: s.SSHHost, StatusPort: s.StatusPort, ControlURL: "http://10.0.0.1:8765", Keys: map[string]string{}}, nil
	}
	dispatch := func(body string) error {
		r := httptest.NewRequest("PUT", "/api/bastion/tailscale/new-share", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Role: "admin", Username: "admin"})
		return err
	}
	if dispatch(`{"enabled":true,"ssh_port":2222}`) == nil {
		t.Fatal("failed SSH accepted")
	}
	var count int
	h.DB.SQL.QueryRow("SELECT count(*) FROM bastion_tailscale WHERE id='new-share'").Scan(&count)
	if count != 0 {
		t.Fatal("failed admission persisted")
	}
	fail = false
	if err := dispatch(`{"enabled":true,"ssh_port":2222}`); err != nil {
		t.Fatal(err)
	}
	s, err := h.share("new-share")
	if err != nil || s.ControlURL != "http://10.0.0.1:8765" {
		t.Fatalf("%+v %v", s, err)
	}
	if err = dispatch(`{"enabled":false}`); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("disable performed SSH check: %d", calls)
	}
	if dispatch(`{"enabled":true,"ssh_host":"100.64.99.99"}`) == nil {
		t.Fatal("foreign IP accepted")
	}
	if err = dispatch(`{"enabled":false,"ssh_port":2222,"status_port":8765}`); err != nil {
		t.Fatal(err)
	}
	s, err = h.share("new-share")
	if err != nil || s.Enabled || calls != 3 {
		t.Fatalf("disabled configuration was not checked and preserved: %+v %d %v", s, calls, err)
	}
}

func TestShareAddressChangeChecksReferencesAfterSSH(t *testing.T) {
	h, f, member := fixture(t)
	if _, err := h.DB.SQL.Exec("UPDATE member_access SET tailscale_id=NULL WHERE member_id=?", member.ID); err != nil {
		t.Fatal(err)
	}
	h.Tailscale = deviceNetwork{f, []tailscale.Device{{NodeID: "node-a", Hostname: "Node A", Authorized: true, Addresses: []string{"100.64.0.3"}}}}
	h.RemoteCommand = func(_ context.Context, s ShareNode, _ commandRequest) (commandReply, error) {
		// Registration can assign the share while SSH admission is in progress.
		if _, err := h.DB.SQL.Exec("UPDATE member_access SET tailscale_id=? WHERE member_id=?", s.ID, member.ID); err != nil {
			t.Fatal(err)
		}
		return commandReply{ControlURL: s.ControlURL}, nil
	}
	r := httptest.NewRequest("PUT", "/api/bastion/tailscale/node-a", strings.NewReader(`{"enabled":true,"ssh_host":"100.64.0.3"}`))
	r.Header.Set("Content-Type", "application/json")
	_, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Role: "admin", Username: "admin"})
	var api *httpapi.Error
	if !errors.As(err, &api) || api.Status != 409 {
		t.Fatalf("changed a referenced share: %v", err)
	}
	s, err := h.share("node-a")
	if err != nil || s.SSHHost != "100.64.0.2" {
		t.Fatalf("rejected update changed the address: %+v %v", s, err)
	}
}
func TestMemberKeysAndRevocationAreScopedToAssignedShare(t *testing.T) {
	h, _, alice := fixture(t)
	store := &members.Store{Database: h.DB}
	invite, err := store.CreateInvitation("test", 10, "admin")
	if err != nil {
		t.Fatal(err)
	}
	register := func(name string) members.Member {
		m, e := store.RegisterWith(members.Registration{Username: name, SSHKey: testKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, h.Reserve)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	bob := register("bob")
	h.DB.SQL.Exec("UPDATE bastion_tailscale SET enabled=0 WHERE id='node-a'")
	if _, err = h.DB.SQL.Exec("INSERT INTO bastion_tailscale VALUES('node-b','Node B',1,'100.64.0.3',22,8765,'http://10.0.0.1:8765')"); err != nil {
		t.Fatal(err)
	}
	charlie := register("charlie")
	var mu sync.Mutex
	pools := map[string]keySnapshot{}
	h.RemoteCommand = func(ctx context.Context, s ShareNode, req commandRequest) (commandReply, error) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := pools[s.ID]
		if !ok {
			v = keySnapshot{Version: keyFormat, Keys: map[string]string{}}
		}
		switch req.Operation {
		case "ensure":
			ensurePoolKeys(&v, req.Keys...)
		case "remove":
			for id, key := range v.Keys {
				if key == req.Key {
					delete(v.Keys, id)
				}
			}
		case "clean":
			delete(v.Keys, req.Entry)
		}
		pools[s.ID] = v
		copy := map[string]string{}
		for id, key := range v.Keys {
			copy[id] = key
		}
		return commandReply{Version: keyFormat, ListenHost: s.SSHHost, StatusPort: s.StatusPort, ControlURL: s.ControlURL, Keys: copy}, nil
	}
	for _, m := range []members.Member{alice, bob, charlie} {
		if err = h.editMemberKey(context.Background(), m.ID, testKey); err != nil {
			t.Fatal(err)
		}
	}
	if len(pools["node-a"].Keys) != 1 || len(pools["node-b"].Keys) != 1 {
		t.Fatal(pools)
	}
	h.DB.SQL.Exec("UPDATE members SET status='deleting' WHERE id=?", alice.ID)
	if err = h.editMemberKey(context.Background(), alice.ID, ""); err != nil {
		t.Fatal(err)
	}
	if len(pools["node-a"].Keys) != 1 {
		t.Fatal("revoked shared key early")
	}
	h.DB.SQL.Exec("UPDATE member_access SET key_state='deleted' WHERE member_id=?", alice.ID)
	h.DB.SQL.Exec("UPDATE members SET status='deleting' WHERE id=?", bob.ID)
	if err = h.editMemberKey(context.Background(), bob.ID, ""); err != nil {
		t.Fatal(err)
	}
	h.DB.SQL.Exec("UPDATE member_access SET key_state='deleted' WHERE member_id=?", bob.ID)
	if len(pools["node-a"].Keys) != 0 || len(pools["node-b"].Keys) != 1 {
		t.Fatal("revocation crossed share nodes")
	}
	freeID := strings.Repeat("f", 32)
	v := pools["node-a"]
	v.Keys[freeID] = managerTestKey
	pools["node-a"] = v
	if err = h.SyncKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pools["node-a"].Keys) != 1 {
		t.Fatal("sync removed free entry")
	}
	keys, problem, err := h.keyPool(context.Background())
	if err != nil || problem != "" || len(keys) != 2 {
		t.Fatalf("%+v %s %v", keys, problem, err)
	}
	var used poolKey
	for _, k := range keys {
		if k.NodeID == "node-b" {
			used = k
		}
	}
	if used.State != "used" || len(used.Members) != 1 || used.Members[0].ID != charlie.ID {
		t.Fatal(used)
	}
	if err = h.cleanFreeKey(context.Background(), "node-b", used.ID, "admin"); err == nil {
		t.Fatal("cleaned referenced key")
	}
	if err = h.cleanFreeKey(context.Background(), "node-a", freeID, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(pools["node-a"].Keys) != 0 {
		t.Fatal("free entry remained")
	}
}
func TestSSHClientUsesWorkerIdentityAndRejectsBadReply(t *testing.T) {
	s := ShareNode{SSHHost: "100.64.0.2", SSHPort: 22, StatusPort: 9765}
	args := strings.Join(sshArgs(s), " ")
	if !strings.Contains(args, "-l alpha-worker") || !strings.Contains(args, "StrictHostKeyChecking=yes") || !strings.Contains(args, "ClearAllForwardings=yes") || strings.Contains(args, "-l root") {
		t.Fatal(args)
	}
	good := commandReply{Version: keyFormat, ListenHost: s.SSHHost, StatusPort: s.StatusPort, ControlURL: "http://10.0.0.1:8765", Keys: map[string]string{}}
	if err := checkReply(s, good); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"version", "host", "port", "target", "keys"} {
		bad := good
		switch kind {
		case "version":
			bad.Version = 1
		case "host":
			bad.ListenHost = "100.64.0.3"
		case "port":
			bad.StatusPort = 12345
		case "target":
			bad.ControlURL = "file:///tmp/secret"
		case "keys":
			bad.Keys = map[string]string{"bad": "bad"}
		}
		if err := checkReply(s, bad); err == nil {
			t.Fatal(kind)
		}
	}
	h, _, _ := fixture(t)
	h.RemoteCommand = func(context.Context, ShareNode, commandRequest) (commandReply, error) {
		return commandReply{}, errors.New("offline")
	}
	keys, problem, err := h.keyPool(context.Background())
	if err != nil || len(keys) != 0 || !strings.Contains(problem, "offline") {
		t.Fatalf("%v %s %v", keys, problem, err)
	}
}

func TestSSHClientOptionsAreAccepted(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh unavailable")
	}
	s := ShareNode{SSHHost: "100.64.0.2", SSHPort: 22, StatusPort: 9765}
	args := append([]string{"-G", "-F", "/dev/null"}, sshArgs(s)...)
	args = append(args, s.SSHHost, "alpha-worker cmd")
	if out, err := exec.Command(ssh, args...).CombinedOutput(); err != nil {
		t.Fatalf("SSH client rejected management options: %v %s", err, out)
	}
}
