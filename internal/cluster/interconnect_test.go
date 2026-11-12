package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

func TestInterconnectPartialRevocation(t *testing.T) {
	f := setup(t)
	var offline atomic.Bool
	var granted [2]atomic.Bool
	var refs []link.Ref
	for i, tag := range []string{"a", "b"} {
		ref := link.Ref{Node: strings.Repeat(tag, 32), Container: strings.Repeat(tag, 64)}
		refs = append(refs, ref)
		var applied atomic.Int64
		w, s := worker(t, ref.Node, Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
			if r.URL.Path != link.WorkerPath {
				return 404, nil, nil
			}
			if u.Role != "admin" {
				return 403, nil, nil
			}
			if r.Method == "GET" {
				return 200, map[string]any{"candidates": []map[string]string{{"id": ref.Container, "name": tag, "owner": tag}}, "applied": applied.Load()}, nil
			}
			var op link.Request
			if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
				return 400, nil, err
			}
			st := link.Status{Protocol: link.Protocol, Interface: "eth0"}
			if op.Operation == "apply" {
				if i == 1 && offline.Load() {
					return 0, nil, httpapi.NewError(503, "offline")
				}
				if err := op.Config.Validate(); err != nil {
					return 0, nil, err
				}
				granted[i].Store(len(op.Config.Connections) != 0)
				applied.Store(op.Config.Revision)
			}
			st.Applied = applied.Load()
			return 200, st, nil
		}))
		requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": []string{"192.0.2.1", "192.0.2.2"}[i], "name": tag, "url": s.URL, "token": w.Token}), 201)
	}
	var view linkView
	change := func(op linkChange, status int) {
		t.Helper()
		op.Revision = view.Revision
		w := f.request(t, "POST", link.ControlPath, op)
		requireStatus(t, w, status)
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ref := range refs {
		change(linkChange{Action: "attach", Endpoint: ref}, 200)
	}
	if len(view.Endpoints) != 2 || view.Endpoints[0].Subnet() == view.Endpoints[1].Subnet() {
		t.Fatal(view)
	}
	change(linkChange{Action: "save", Connection: link.Connection{A: refs[0], B: refs[1], Enabled: true, Rules: []link.Rule{{Direction: "ab", Protocol: "tcp", First: 81, Last: 85, Enabled: true}}}}, 200)
	if !granted[0].Load() || !granted[1].Load() {
		t.Fatal("both workers must start with an active grant")
	}
	id := view.Connections[0].ID
	offline.Store(true)
	change(linkChange{Action: "disable", ID: id}, 200)
	if view.Nodes[0].Applied != view.Revision || view.Nodes[1].Applied >= view.Revision || view.Nodes[1].Error == "" {
		t.Fatal(view.Nodes)
	}
	if granted[0].Load() || !granted[1].Load() {
		t.Fatal("only the offline worker should retain its previous grant")
	}
	change(linkChange{Action: "delete", ID: id}, 200)
	if len(view.Connections) != 1 || !view.Connections[0].Removing {
		t.Fatal("lost tombstone")
	}
	change(linkChange{Action: "detach", Endpoint: refs[0]}, 409)
	requireStatus(t, f.request(t, "DELETE", "/api/cluster/nodes/"+refs[1].Node, nil), 409)
	offline.Store(false)
	// No overview page, manual reconnect or retry request is open. The
	// outstanding revocation must reach the returning worker on its own.
	deadline := time.Now().Add(12 * time.Second)
	for {
		var pending int
		if err := f.db.SQL.QueryRow("SELECT count(*) FROM link_connections").Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("returning worker did not acknowledge the revocation")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if granted[1].Load() {
		t.Fatal("returning worker retained the previous grant")
	}
	offline.Store(true)
	change(linkChange{Action: "detach", Endpoint: refs[1]}, 200)
	if len(view.Endpoints) != 2 {
		t.Fatal("address reused before ack")
	}
	offline.Store(false)
	change(linkChange{Action: "retry"}, 200)
	if len(view.Endpoints) != 1 {
		t.Fatal(view.Endpoints)
	}
}

func TestInterconnectIPv6WorkerCanCorrectAddress(t *testing.T) {
	f := setup(t)
	d := addDeliveryWorker(t, f, "a", "fd00::42")
	resp := f.request(t, "POST", link.ControlPath, linkChange{Action: "attach", Endpoint: d.ref})
	requireStatus(t, resp, 409)
	if !strings.Contains(resp.Body.String(), "IPv4") {
		t.Fatal(resp.Body.String())
	}
	var allocations, rev int
	if err := f.db.SQL.QueryRow("SELECT (SELECT count(*) FROM link_nodes)+(SELECT count(*) FROM link_endpoints),revision FROM link_settings").Scan(&allocations, &rev); err != nil || allocations != 0 || rev != 0 {
		t.Fatalf("invalid attach persisted allocations=%d rev=%d err=%v", allocations, rev, err)
	}
	// An already reserved IPv6 endpoint can still be withdrawn with an
	// authenticated empty snapshot before correcting the node address.
	if _, err := f.db.SQL.Exec("INSERT INTO link_nodes(node,subnet,desired) VALUES(?,'10.240.0.0/24',1)", d.ref.Node); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.Exec("INSERT INTO link_endpoints(node,container,ip,name,owner) VALUES(?,?,'10.240.0.2','one','alice')", d.ref.Node, d.ref.Container); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.Exec("UPDATE link_settings SET revision=1"); err != nil {
		t.Fatal(err)
	}
	v := changeLinks(t, f, linkChange{Action: "detach", Endpoint: d.ref})
	if len(v.Endpoints) != 0 || v.Nodes[0].Applied != v.Revision {
		t.Fatal(v)
	}
	n, err := f.control.node(d.ref.Node)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "PUT", "/api/cluster/nodes/"+n.ID, nodeInput{Kind: n.Kind, Name: n.Name, URL: n.URL, InternalIP: "192.0.2.42"}), 200)
	v = changeLinks(t, f, linkChange{Action: "attach", Endpoint: d.ref})
	if len(v.Endpoints) != 1 || v.Nodes[0].Applied != v.Revision || v.Nodes[0].Error != "" {
		t.Fatal(v)
	}
}
