package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

func TestInvitationNodePermissionsAndDeferredContainers(t *testing.T) {
	f := setup(t)
	var allocations atomic.Int32
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		if r.URL.Path == "/api/containers/candidates" {
			return 200, map[string]any{"containers": []containerCandidate{}}, nil
		}
		if strings.HasPrefix(r.URL.Path, "/api/containers/members/") {
			allocations.Add(1)
			return 200, map[string]any{"id": strings.Repeat("c", 64), "name": "workspace", "port": 2222}, nil
		}
		return 200, map[string]bool{"ok": true}, nil
	})
	a, as := worker(t, strings.Repeat("a", 32), Inventory{}, module)
	add(t, f, a, as, "Allowed")
	b, bs := worker(t, strings.Repeat("b", 32), Inventory{}, module)
	add(t, f, b, bs, "Forbidden")
	store := &members.Store{Database: f.db}
	p := members.NodePermissions{NodeIDs: []string{a.ID}}
	invite, err := store.CreateInvitation("limited", 2, "admin", p)
	if err != nil {
		t.Fatal(err)
	}
	options, err := f.control.RegistryDispatch(context.Background(), registry.Request{Action: "options", Invitation: invite.Code, Body: json.RawMessage(`{"username":"alice"}`)})
	if err != nil {
		t.Fatal(err)
	}
	nodes := options.(map[string]any)["nodes"].([]nodeChoices)
	if len(nodes) != 1 || nodes[0].NodeID != a.ID {
		t.Fatal("unauthorized registration options", nodes)
	}
	w := selfCall(f, "POST", "/api/members/registration-options", "", map[string]string{"invitation_code": invite.Code, "username": "alice"})
	requireStatus(t, w, 200)
	if strings.Contains(w.Body.String(), b.ID) {
		t.Fatal("public options leaked unauthorized node")
	}
	req := members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: resourceTestKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}
	for _, mode := range []string{"create", "skip", "adopt"} {
		choice := members.ContainerChoice{NodeID: b.ID, Mode: mode}
		if mode == "adopt" {
			choice.ContainerID = strings.Repeat("d", 64)
		}
		req.Containers = []members.ContainerChoice{choice}
		if _, err = store.RegisterWith(req, f.control.Members.Reserve); err == nil {
			t.Fatal("registered unauthorized node", mode)
		}
	}
	var used int
	if err = f.db.SQL.QueryRow("SELECT used FROM member_invitations WHERE id=?", invite.ID).Scan(&used); err != nil || used != 0 {
		t.Fatal("failed registration consumed invitation", err)
	}
	req.Containers = []members.ContainerChoice{{NodeID: a.ID, Mode: "skip"}}
	token := strings.Repeat("e", 64)
	m, err := f.control.Members.RegisterRegistry(req, token)
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, f, m.ID)
	if allocations.Load() != 0 {
		t.Fatal("skip allocated a container")
	}
	if _, err = f.control.Members.RegisterRegistry(req, token); err != nil {
		t.Fatal("idempotent skip registration", err)
	}
	req.Containers[0].Mode = "create"
	if _, err = f.control.Members.RegisterRegistry(req, token); err == nil {
		t.Fatal("retry changed registration")
	}
	awaitIdle(t, f, m.ID)
	for _, mode := range []string{"create", "adopt"} {
		body := map[string]string{"node_id": b.ID, "mode": mode}
		if mode == "adopt" {
			body["container_id"] = strings.Repeat("d", 64)
		}
		requireStatus(t, selfCall(f, "POST", "/api/members/me/containers", token, body), 403)
		requireStatus(t, f.request(t, "POST", "/api/members/"+m.ID+"/containers", body), 403)
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+b.ID+"/api/containers", map[string]string{"owner": "alice"}), 403)
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+b.ID+"/api/owners", map[string]string{"owner": "alice", "id": strings.Repeat("d", 64)}), 403)
	requireStatus(t, selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": a.ID, "mode": "create"}), 200)
	awaitIdle(t, f, m.ID)
	if allocations.Load() != 1 {
		t.Fatal("deferred allocation did not run exactly once")
	}
	path := "/api/members/" + m.ID + "/node-permissions"
	requireStatus(t, f.request(t, "PUT", path, members.NodePermissions{NodeIDs: []string{strings.Repeat("9", 32)}}), 400)
	requireStatus(t, f.request(t, "PUT", path, members.NodePermissions{NodeIDs: []string{}}), 200)
	requireStatus(t, selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": a.ID, "mode": "create"}), 403)
	resources, err := f.control.memberResources(m.ID)
	if err != nil || len(resources.Nodes) != 1 || resources.Nodes[0].Allowed || resources.Nodes[0].ContainerID == "" {
		t.Fatal("revocation lost existing container", resources, err)
	}
	if allocations.Load() != 1 {
		t.Fatal("unauthorized allocation reached worker")
	}
	if _, err = f.db.SQL.Exec("INSERT INTO member_node_resources(member_id,node_id,state,updated_at) VALUES(?,?,'pending',?)", m.ID, b.ID, platform.Now()); err != nil {
		t.Fatal(err)
	}
	gate := f.control.memberGate(m.ID)
	gate.Lock()
	err = f.control.applyMemberNode(context.Background(), m.ID, b.ID, "alice", resourceTestKey, false)
	gate.Unlock()
	if err == nil || allocations.Load() != 1 {
		t.Fatal("revoked queued allocation reached worker", err)
	}
	var pendingState string
	if err = f.db.SQL.QueryRow("SELECT state FROM member_node_resources WHERE member_id=? AND node_id=?", m.ID, b.ID).Scan(&pendingState); err != nil || pendingState != "failed" {
		t.Fatal("revoked work was not marked failed", err)
	}
	requireStatus(t, f.request(t, "PUT", path, members.NodePermissions{AllNodes: true, NodeIDs: []string{}}), 200)
	if err = f.control.requireMemberNode(m.ID, b.ID); err != nil {
		t.Fatal("grant did not take effect", err)
	}
	// Invitation permissions are copied at registration, independently of later edits or deletion.
	if err = store.DeleteInvitation(invite.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = f.control.requireMemberNode(m.ID, b.ID); err != nil {
		t.Fatal("invitation deletion removed member permissions", err)
	}
}

func TestRegistrationWithNoAuthorizedNodes(t *testing.T) {
	f := setup(t)
	store := &members.Store{Database: f.db}
	i, err := store.CreateInvitation("no containers", 1, "admin", members.NodePermissions{NodeIDs: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.control.Members.RegisterRegistry(members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: resourceTestKey, InvitationCode: i.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}, Containers: []members.ContainerChoice{}}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, f, m.ID)
	w := f.request(t, "GET", "/api/members/"+m.ID+"/node-permissions", nil)
	requireStatus(t, w, 200)
	if strings.Contains(w.Body.String(), `"all_nodes":true`) {
		t.Fatal("empty policy granted all nodes")
	}
	f.user.Role = "viewer"
	_, _, err = f.control.Dispatch(nil, httptest.NewRequest("PUT", "/api/members/"+m.ID+"/node-permissions", nil), f.user)
	if err == nil {
		t.Fatal("viewer changed permissions")
	}
}

func TestRevokedManualContainerPreservesNodeMetadata(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	var candidateCalls atomic.Int32
	module := moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		if r.URL.Path == "/api/containers/candidates" {
			candidateCalls.Add(1)
			return 200, map[string]any{"containers": []containerCandidate{{ID: strings.Repeat("c", 64), Name: "candidate"}}}, nil
		}
		return 200, map[string]bool{"ok": true}, nil
	})
	container := Container{ID: strings.Repeat("d", 64), Name: "manually-assigned", Owner: "alice", Managed: true}
	node, server := worker(t, strings.Repeat("a", 32), Inventory{Containers: []Container{container}}, module)
	add(t, f, node, server, "Manual worker")
	other, otherServer := worker(t, strings.Repeat("b", 32), Inventory{Containers: []Container{{ID: strings.Repeat("e", 64), Name: "other-container", Owner: "bob", Managed: true}}}, module)
	add(t, f, other, otherServer, "Other worker")
	awaitIdle(t, f, id)
	// Administrator assignments have no member_node_resources slot, but key
	// synchronization still records results for the member's existing containers.
	if _, err := f.db.SQL.Exec("INSERT INTO member_key_sync(member_id,node_id,state,error) VALUES(?,?,'failed','key delivery failed')", id, node.ID); err != nil {
		t.Fatal(err)
	}
	readStatus := func() []memberNodeStatus {
		t.Helper()
		response := selfCall(f, "GET", "/api/status/alice", token, nil)
		requireStatus(t, response, 200)
		var view struct {
			Nodes []memberNodeStatus `json:"nodes"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view.Nodes
	}
	before := readStatus()
	if len(before) != 2 || candidateCalls.Load() != 2 {
		t.Fatal("authorized nodes or candidates missing", before, candidateCalls.Load())
	}
	requireStatus(t, f.request(t, "PUT", "/api/members/"+id+"/node-permissions", members.NodePermissions{NodeIDs: []string{}}), 200)
	after := readStatus()
	if len(after) != 1 || after[0].NodeID != node.ID || len(after[0].Containers) != 1 || after[0].Containers[0].ID != container.ID {
		t.Fatalf("revocation must retain only the node with an existing member container: %+v", after)
	}
	n := after[0]
	if n.InternalIP != "10.0.0.11" || n.KeyState != "failed" || n.KeyError != "key delivery failed" {
		t.Fatalf("revocation discarded node metadata: %+v", n)
	}
	if n.Allowed || n.State != "unallocated" || len(n.Candidates) != 0 || candidateCalls.Load() != 2 {
		t.Fatalf("revoked node still permits allocation or candidate queries: %+v", n)
	}
	// Resource responses do not query live inventory and must still omit
	// unauthorized nodes without a provisioning slot.
	response := selfCall(f, "GET", "/api/members/me/resources", resourceToken, nil)
	requireStatus(t, response, 200)
	var resources memberResourceView
	if err := json.Unmarshal(response.Body.Bytes(), &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.Nodes) != 0 {
		t.Fatal("resource response exposed unauthorized unallocated nodes", resources.Nodes)
	}
}
