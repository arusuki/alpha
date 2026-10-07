package cluster

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func TestOverviewCountsMembersInsteadOfImportedContainerNames(t *testing.T) {
	f := setup(t)
	store := &members.Store{Database: f.db}
	invite, err := store.CreateInvitation("overview", 2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"alice", "unused"} {
		_, err := store.RegisterWith(members.Registration{Username: username, Password: "Member-password-123", SSHKey: resourceTestKey, InvitationCode: invite.Code, SchemaRevision: 1, Profile: map[string]json.RawMessage{}}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	containers := []Container{{ID: "assigned", Name: "alice-workspace", Owner: "alice", Managed: true}}
	for i := range 18 {
		name := fmt.Sprintf("imported-%02d", i)
		containers = append(containers, Container{ID: name, Name: name, Owner: name, Managed: true})
	}
	// Blank ownership and unregistered name hints belong in the same group.
	containers[18].Owner = ""
	w, server := worker(t, strings.Repeat("a", 32), Inventory{Containers: containers}, nil)
	add(t, f, w, server, "worker")
	for _, role := range []string{"admin", "viewer"} {
		t.Run(role, func(t *testing.T) {
			code, value, err := f.control.overview(httptest.NewRequest("GET", "/api/cluster/overview", nil), platform.User{ID: f.user.ID, Username: role, Role: role})
			if err != nil || code != 200 {
				t.Fatalf("overview: %d %v", code, err)
			}
			data := value.(map[string]any)
			if data["container_count"] != 19 || data["partial"] != false {
				t.Fatalf("wrong totals: %+v", data)
			}
			groups := data["members"].([]*MemberSummary)
			wantGroups := 2
			if role == "admin" {
				wantGroups++
			}
			if len(groups) != wantGroups {
				t.Fatalf("invented members: %+v", groups)
			}
			for _, m := range groups {
				switch m.Username {
				case "alice":
					if m.Count != 1 || !m.Registered || len(m.Nodes) != 1 {
						t.Fatalf("wrong assigned count: %+v", m)
					}
				case "":
					if m.Count != 18 || m.Registered || m.ID != "" || len(m.Nodes) != 1 {
						t.Fatalf("wrong unassigned group: %+v", m)
					}
				case "unused":
					if role != "admin" || m.Count != 0 || !m.Registered {
						t.Fatalf("unused member visibility: %+v", m)
					}
				default:
					t.Fatalf("container name counted as member: %+v", m)
				}
				if role == "viewer" && m.ID != "" {
					t.Fatal("viewer received member management ID")
				}
			}
			assigned := 0
			for _, c := range data["nodes"].([]NodeStatus)[0].Inventory.Containers {
				if c.Owner != "" {
					assigned++
					if c.Owner != "alice" {
						t.Fatalf("node card ownership differs from summary: %+v", c)
					}
				}
			}
			if assigned != 1 {
				t.Fatalf("node assigned count: %d", assigned)
			}
		})
	}
	// Aggregation must not overwrite worker ownership metadata used by adoption.
	if containers[1].Owner != "imported-00" {
		t.Fatal("aggregation modified worker records")
	}
}
