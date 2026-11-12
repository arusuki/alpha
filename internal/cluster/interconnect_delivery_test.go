package cluster

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

type deliveryWorker struct {
	sync.Mutex
	ref                        link.Ref
	desired, applied           int64
	failure                    string
	config                     string
	dropACK, fail, uninstalled bool
	applies                    int
	applying                   int64
	executions, gets           int
	forceExecutions            int
	block                      <-chan struct{}
	started                    chan struct{}
	drift, busy                bool
}

func (d *deliveryWorker) dispatch(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
	if r.URL.Path != link.WorkerPath {
		return 404, nil, nil
	}
	d.Lock()
	defer d.Unlock()
	if r.Method == "GET" {
		d.gets++
		return 200, map[string]any{"candidates": []map[string]string{{"id": d.ref.Container, "name": "one", "owner": "alice"}}, "desired": d.desired, "applied": d.applied, "error": d.failure, "applying": d.applying, "config_digest": fmt.Sprintf("%x", sha256.Sum256([]byte(d.config)))}, nil
	}
	var req link.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return 400, nil, err
	}
	if req.Operation == "check" && d.drift {
		return 0, nil, httpapi.NewError(409, "互联网络发生漂移")
	}
	if req.Operation == "apply" {
		d.applies++
		if d.busy {
			return 0, nil, httpapi.NewError(503, "worker 正忙")
		}
		if d.uninstalled {
			return 0, nil, httpapi.NewError(409, "helper 已卸载")
		}
		if req.Config.Revision < d.desired {
			return 0, nil, httpapi.NewError(409, "配置版本过期")
		}
		raw := httpapi.JSONText(req.Config)
		if req.Config.Revision == d.desired && raw != d.config {
			return 0, nil, httpapi.NewError(409, "相同版本内容不一致")
		}
		if !req.Force && d.desired == req.Config.Revision && d.applied == d.desired && d.failure == "" {
			return 200, link.Status{Protocol: link.Protocol, Applied: d.applied}, nil
		}
		d.desired = req.Config.Revision
		d.config = raw
		d.executions++
		if req.Force {
			d.forceExecutions++
		}
		if d.block != nil {
			gate := d.block
			d.block = nil
			d.applying = req.Config.Revision
			d.failure = "正在应用"
			if d.started != nil {
				close(d.started)
				d.started = nil
			}
			d.Unlock()
			<-gate
			d.Lock()
			d.applying = 0
		}
		if d.fail {
			d.failure = "helper 应用失败"
			return 0, nil, httpapi.NewError(409, d.failure)
		}
		d.applied, d.failure = d.desired, ""
		d.drift = false
		if d.dropACK {
			d.dropACK = false
			return 0, nil, httpapi.NewError(503, "apply response lost")
		}
	}
	return 200, link.Status{Protocol: link.Protocol, Interface: "eth0", Applied: d.applied}, nil
}

func addDeliveryWorker(t *testing.T, f *fixture, tag, internal string) *deliveryWorker {
	t.Helper()
	d := &deliveryWorker{ref: link.Ref{Node: strings.Repeat(tag, 32), Container: strings.Repeat(tag, 64)}}
	w, s := worker(t, d.ref.Node, Inventory{}, moduleFunc(d.dispatch))
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes", map[string]string{"kind": "worker", "internal_ip": internal, "name": tag, "url": s.URL, "token": w.Token}), 201)
	return d
}

func changeLinks(t *testing.T, f *fixture, op linkChange) linkView {
	t.Helper()
	if err := f.db.SQL.QueryRow("SELECT revision FROM link_settings").Scan(&op.Revision); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "POST", link.ControlPath, op)
	requireStatus(t, r, 200)
	var v linkView
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInterconnectRecoversDurableAcknowledgements(t *testing.T) {
	f := setup(t)
	a := addDeliveryWorker(t, f, "a", "192.0.2.1")
	b := addDeliveryWorker(t, f, "b", "192.0.2.2")
	for _, d := range []*deliveryWorker{a, b} {
		changeLinks(t, f, linkChange{Action: "attach", Endpoint: d.ref})
	}
	v := changeLinks(t, f, linkChange{Action: "save", Connection: link.Connection{A: a.ref, B: b.ref, Enabled: true, Rules: []link.Rule{{Direction: "ab", Protocol: "all", Enabled: true}}}})
	b.Lock()
	b.dropACK = true
	want := b.applies + 1
	b.Unlock()
	v = changeLinks(t, f, linkChange{Action: "delete", ID: v.Connections[0].ID})
	if len(v.Connections) != 1 || !v.Connections[0].Removing {
		t.Fatal("lost response released the connection before durable acknowledgement")
	}
	v = changeLinks(t, f, linkChange{Action: "retry"})
	b.Lock()
	got := b.applies
	b.Unlock()
	if got != want || len(v.Connections) != 0 {
		t.Fatalf("retry repeated a completed apply or retained its tombstone: applies=%d want=%d, connections=%v", got, want, v.Connections)
	}

	b.Lock()
	b.dropACK = true
	want = b.applies + 1
	b.Unlock()
	v = changeLinks(t, f, linkChange{Action: "detach", Endpoint: b.ref})
	if len(v.Endpoints) != 2 {
		t.Fatal("lost response released the endpoint allocation")
	}
	v = changeLinks(t, f, linkChange{Action: "retry"})
	b.Lock()
	got = b.applies
	b.uninstalled = true
	b.Unlock()
	if got != want || len(v.Endpoints) != 1 {
		t.Fatalf("retry repeated a completed withdrawal or retained its allocation: applies=%d want=%d, endpoints=%v", got, want, v.Endpoints)
	}
	b.Lock()
	b.failure = "helper 已卸载，历史空配置重放失败"
	b.Unlock()
	if _, err := f.db.SQL.Exec("UPDATE link_nodes SET error='历史空配置重放失败' WHERE node=?", b.ref.Node); err != nil {
		t.Fatal(err)
	}
	v = changeLinks(t, f, linkChange{Action: "retry"})
	for _, n := range v.Nodes {
		if n.ID == b.ref.Node && n.Error != "" {
			t.Fatal("retired worker's stale failure reappeared in the view", n.Error)
		}
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/nodes/"+b.ref.Node+"/reconnect", nil), 200)
	b.Lock()
	got = b.applies
	b.Unlock()
	var failure string
	if err := f.db.SQL.QueryRow("SELECT error FROM link_nodes WHERE node=?", b.ref.Node).Scan(&failure); err != nil {
		t.Fatal(err)
	}
	if got != want || failure != "" {
		t.Fatalf("retired node received an empty apply after helper uninstall: applies=%d want=%d, error=%q", got, want, failure)
	}
}

func TestInterconnectRetriesUnfinishedWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sameApplied bool
		newerIntent bool
	}{{name: "unapplied"}, {name: "applied-with-error", sameApplied: true}, {name: "newer-intent", sameApplied: true, newerIntent: true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			d := addDeliveryWorker(t, f, "a", "192.0.2.1")
			changeLinks(t, f, linkChange{Action: "attach", Endpoint: d.ref})
			d.Lock()
			d.fail = true
			d.Unlock()
			v := changeLinks(t, f, linkChange{Action: "detach", Endpoint: d.ref})
			if len(v.Endpoints) != 1 || !v.Endpoints[0].Removing {
				t.Fatal("failed withdrawal released its allocation")
			}
			d.Lock()
			d.fail = false
			if tc.sameApplied {
				// A failure of a later replay can retain the last applied
				// counter. The error still requires a real retry.
				d.applied = d.desired
			}
			if tc.newerIntent {
				d.desired++
				d.failure = ""
			}
			want := d.applies + 1
			d.Unlock()
			if tc.sameApplied && !tc.newerIntent {
				if _, err := f.db.SQL.Exec("UPDATE link_nodes SET applied=desired WHERE node=?", d.ref.Node); err != nil {
					t.Fatal(err)
				}
			}
			v = changeLinks(t, f, linkChange{Action: "retry"})
			d.Lock()
			got := d.applies
			d.Unlock()
			if tc.newerIntent {
				if got != want || len(v.Endpoints) != 1 || !v.Endpoints[0].Removing || !strings.Contains(v.Nodes[0].Error, "配置版本过期") {
					t.Fatalf("a different desired revision was incorrectly acknowledged: applies=%d want=%d, view=%+v", got, want, v)
				}
				return
			}
			if got != want || len(v.Endpoints) != 0 || v.Nodes[0].Error != "" {
				t.Fatalf("unfinished withdrawal was not retried: applies=%d want=%d, view=%+v", got, want, v)
			}
		})
	}
}

func awaitLinkDelivery(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(description)
}

func TestInterconnectSlowHTTPApplyConvergesWithoutDuplicates(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "new-revision-withdrawal"
		if force {
			name = "force-repair"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.control.client.Timeout = 100 * time.Millisecond
			d := addDeliveryWorker(t, f, "a", "192.0.2.1")
			changeLinks(t, f, linkChange{Action: "attach", Endpoint: d.ref})
			release, started := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			finish := func() { unblock.Do(func() { close(release) }) }
			t.Cleanup(finish)
			d.Lock()
			d.block, d.started = release, started
			wantExecutions := d.executions + 1
			if force {
				d.drift, d.busy = true, true
			}
			d.Unlock()
			if force {
				// The first repair cannot start and leaves the worker's old
				// successful ACK intact. It must not hide the runtime drift.
				v := changeLinks(t, f, linkChange{Action: "retry"})
				if v.Nodes[0].Error == "" {
					t.Fatal("busy force replay was incorrectly acknowledged")
				}
				d.Lock()
				d.busy = false
				d.Unlock()
			} else {
				changeLinks(t, f, linkChange{Action: "detach", Endpoint: d.ref})
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("outstanding slow apply never started")
			}
			// For force, the automatic retry is still holding the control
			// lock until this real HTTP timeout elapses.
			for i := 0; i < 3; i++ {
				changeLinks(t, f, linkChange{Action: "retry"})
			}
			d.Lock()
			wantRequests, priorGets := d.applies, d.gets
			d.Unlock()
			awaitLinkDelivery(t, "automatic recovery did not poll an in-flight request", func() bool {
				d.Lock()
				defer d.Unlock()
				return d.gets > priorGets
			})
			d.Lock()
			gotRequests, gotExecutions, running := d.applies, d.executions, d.applying
			d.Unlock()
			if gotRequests != wantRequests || gotExecutions != wantExecutions || running == 0 {
				t.Fatal("in-flight retries queued duplicate applies", gotRequests, wantRequests, gotExecutions, wantExecutions, running)
			}
			finish()
			awaitLinkDelivery(t, "background recovery did not confirm the original slow apply", func() bool {
				var desired, applied int64
				var failure string
				if err := f.db.SQL.QueryRow("SELECT desired,applied,error FROM link_nodes WHERE node=?", d.ref.Node).Scan(&desired, &applied, &failure); err != nil {
					t.Fatal(err)
				}
				return desired == applied && failure == ""
			})
			d.Lock()
			gotExecutions, forceExecutions := d.executions, d.forceExecutions
			d.Unlock()
			if gotExecutions != wantExecutions || force && forceExecutions != 1 {
				t.Fatal("successful slow request was replayed after confirmation", gotExecutions, wantExecutions, forceExecutions)
			}
			if !force {
				var count int
				if err := f.db.SQL.QueryRow("SELECT count(*) FROM link_endpoints").Scan(&count); err != nil || count != 0 {
					t.Fatal("confirmed withdrawal retained its allocation", count, err)
				}
			}
		})
	}
}
