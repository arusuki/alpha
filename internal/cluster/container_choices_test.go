package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func TestRegistrationNodeChoicesAndClaimedVisibility(t *testing.T) {
	f := setup(t)
	target := strings.Repeat("a", 64)
	occupied := strings.Repeat("b", 64)
	var mu sync.Mutex
	var requests []map[string]string
	module := moduleFunc(func(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if r.URL.Path == "/api/containers/candidates" {
			return 200, map[string]any{"containers": []containerCandidate{{ID: target, Name: "existing", Owner: "legacy"}, {ID: occupied, Name: "taken", Owner: "other", MemberID: strings.Repeat("9", 32)}}}, nil
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return 0, nil, err
		}
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		cid := req["container_id"]
		if req["mode"] == "create" {
			cid = strings.Repeat("c", 64)
		}
		return 200, map[string]any{"id": cid, "name": "workspace", "port": 2222}, nil
	})
	a, as := worker(t, strings.Repeat("1", 32), Inventory{}, module)
	add(t, f, a, as, "Adopt node")
	b, bs := worker(t, strings.Repeat("2", 32), Inventory{}, module)
	add(t, f, b, bs, "Create node")
	options, err := f.control.registrationOptions(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	var view struct {
		Nodes []nodeChoices `json:"nodes"`
	}
	json.Unmarshal(raw, &view)
	if len(view.Nodes) != 2 || len(view.Nodes[0].Containers) != 2 {
		t.Fatalf("claimed entries were hidden: %s", raw)
	}
	for _, n := range view.Nodes {
		if n.Containers[0].Claimed || !n.Containers[1].Claimed {
			t.Fatalf("wrong disabled state: %+v", n)
		}
	}
	store := &members.Store{Database: f.db}
	invite, err := store.CreateInvitation("choices", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := members.Registration{Username: "alice", Password: "Member-password-123", SSHKey: resourceTestKey, SchemaRevision: 1, Profile: map[string]json.RawMessage{}, InvitationCode: invite.Code}
	// Missing, duplicate, or unknown node choices must not consume the invitation.
	for _, choices := range [][]members.ContainerChoice{nil, {{NodeID: a.ID, Mode: "create"}, {NodeID: a.ID, Mode: "create"}}, {{NodeID: a.ID, Mode: "create"}, {NodeID: strings.Repeat("3", 32), Mode: "create"}}} {
		req.Containers = choices
		if _, err = store.RegisterWith(req, f.control.Members.Reserve); err == nil {
			t.Fatal("accepted invalid choices")
		}
	}
	var used int
	f.db.SQL.QueryRow("SELECT used FROM member_invitations WHERE id=?", invite.ID).Scan(&used)
	if used != 0 {
		t.Fatal("invalid choices consumed invitation")
	}
	req.Containers = []members.ContainerChoice{{NodeID: a.ID, Mode: "adopt", ContainerID: target}, {NodeID: b.ID, Mode: "create"}}
	token := strings.Repeat("4", 64)
	m, err := f.control.Members.RegisterRegistry(req, token)
	if err != nil {
		t.Fatal(err)
	}
	awaitResource(t, f, m.ID, a.ID, "ready")
	awaitResource(t, f, m.ID, b.ID, "ready")
	awaitIdle(t, f, m.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("wrong requests: %+v", requests)
	}
	modes := map[string]bool{}
	for _, r := range requests {
		modes[r["mode"]] = true
		if r["mode"] == "adopt" && r["container_id"] != target {
			t.Fatal("wrong target")
		}
	}
	if !modes["create"] || !modes["adopt"] {
		t.Fatal("choices not forwarded")
	}
	if _, err = f.control.Members.RegisterRegistry(req, token); err != nil {
		t.Fatalf("same registration retry: %v", err)
	}
	req.Containers[0].ContainerID = occupied
	if _, err = f.control.Members.RegisterRegistry(req, token); err == nil {
		t.Fatal("retry changed saved choice")
	}
	if _, err = f.control.checkAdoption(context.Background(), Node{ID: a.ID, URL: as.URL, Token: a.Token}, "alice", m.ID, occupied); err == nil {
		t.Fatal("adopted claimed candidate")
	}
	// Retry API must not silently change a persisted selection.
	response := selfCall(f, "POST", "/api/members/me/containers", token, map[string]string{"node_id": a.ID, "mode": "create"})
	requireStatus(t, response, 409)
}
