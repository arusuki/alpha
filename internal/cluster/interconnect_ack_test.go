package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/containers"
	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

func TestInterconnectAcknowledgementRequiresExactPersistedConfig(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "pending-withdrawal"
		if force {
			name = "force-replay"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			db, err := platform.OpenDatabase(t.TempDir(), containers.Initialize)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.SQL.Close() })
			id := strings.Repeat("a", 32)
			e := link.Endpoint{Ref: link.Ref{Node: id, Container: strings.Repeat("b", 64)}, IP: netip.MustParseAddr("10.240.0.2"), Internal: netip.MustParseAddr("192.0.2.1"), Name: "one", Owner: "alice"}
			old := link.Config{Protocol: link.Protocol, Node: id, Revision: 7, Pool: netip.MustParsePrefix("10.240.0.0/16"), Subnet: netip.MustParsePrefix("10.240.0.0/24"), Internal: e.Internal, Endpoints: []link.Endpoint{e}}
			if force {
				old.Endpoints[0].IP = netip.MustParseAddr("10.240.0.6")
			}
			if err := old.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL.Exec("INSERT INTO service_identity VALUES(1,'worker',?)", id); err != nil {
				t.Fatal(err)
			}
			raw := httpapi.JSONText(old)
			if _, err := db.SQL.Exec("UPDATE link_local SET desired=?,applied=?,config=?,error='' WHERE id=1", old.Revision, old.Revision, raw); err != nil {
				t.Fatal(err)
			}
			handler := containers.NewHandler(db)
			var checks atomic.Int64
			w, s := worker(t, id, Inventory{}, moduleFunc(func(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
				if r.Method == "POST" {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return 0, nil, err
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var req link.Request
					if err := json.Unmarshal(body, &req); err != nil {
						return 0, nil, err
					}
					if req.Operation == "check" {
						// The earlier, different policy can be perfectly healthy.
						// Health cannot acknowledge another payload at this version.
						checks.Add(1)
						return 200, link.Status{Protocol: link.Protocol, Applied: old.Revision}, nil
					}
				}
				return handler.Dispatch(w, r, u)
			}))
			requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": e.Internal.String(), "name": "worker", "url": s.URL, "token": w.Token}), 201)
			applied := old.Revision - 1
			if force {
				applied = old.Revision
			}
			f.control.linkMu.Lock()
			err = f.db.Transaction(func(tx *sql.Tx) error {
				if _, err := tx.Exec("INSERT INTO link_nodes(node,subnet,desired,applied,error) VALUES(?,?,?,?,'需要重试')", id, old.Subnet.String(), old.Revision, applied); err != nil {
					return err
				}
				if _, err := tx.Exec("INSERT INTO link_endpoints(node,container,ip,name,owner,removing) VALUES(?,?,?,?,?,?)", id, e.Container, e.IP.String(), e.Name, e.Owner, !force); err != nil {
					return err
				}
				_, err := tx.Exec("UPDATE link_settings SET revision=?", old.Revision)
				return err
			})
			f.control.linkMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			v, err := f.control.linkView()
			if err != nil {
				t.Fatal(err)
			}
			n := v.Nodes[0]
			var apiErr *httpapi.Error
			err = f.control.deliverLink(context.Background(), n, v.config(n))
			if !errors.As(err, &apiErr) || apiErr.Status != 409 || !strings.Contains(err.Error(), "相同版本内容不一致") {
				t.Fatal("different policy at the same version was acknowledged", err)
			}
			if checks.Load() != 0 {
				t.Fatal("runtime health was checked before matching the persisted payload")
			}
			v = changeLinks(t, f, linkChange{Action: "retry"})
			if len(v.Endpoints) != 1 || !strings.Contains(v.Nodes[0].Error, "相同版本内容不一致") {
				t.Fatal("conflicting acknowledgement released an allocation or cleared its error", v)
			}
			var saved, failure string
			var revision int64
			if err := db.SQL.QueryRow("SELECT applied,config,error FROM link_local").Scan(&revision, &saved, &failure); err != nil {
				t.Fatal(err)
			}
			if revision != old.Revision || saved != raw || failure != "" {
				t.Fatal("conflicting retry changed the worker's existing policy", revision, saved, failure)
			}
		})
	}
}
