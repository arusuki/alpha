package containers

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	link "project-alpha/internal/interconnect"
)

func TestInterconnectDurableIntentAndIdentity(t *testing.T) {
	h, d, _ := fixture(t)
	adopt(t, h)
	records, err := h.records()
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	id := strings.Repeat("b", 32)
	e := link.Endpoint{Ref: link.Ref{Node: id, Container: d.c.ID}, Name: "alice", Owner: records[0].Owner, IP: netip.MustParseAddr("10.240.0.2"), Internal: netip.MustParseAddr("192.0.2.1")}
	c := link.Config{Protocol: link.Protocol, Node: id, Revision: 1, Pool: netip.MustParsePrefix("10.240.0.0/16"), Subnet: netip.MustParsePrefix("10.240.0.0/24"), Internal: e.Internal, Endpoints: []link.Endpoint{e}}
	// Interrupt the helper after the durable intent and claims have been committed.
	interrupted := errors.New("interrupted")
	var guards int
	h.linkHelper = func(ctx context.Context, req link.Request) (link.Status, error) {
		if req.Operation == "guard" {
			guards++
			return link.Status{Protocol: link.Protocol}, nil
		}
		var pending bool
		if err := h.db.SQL.QueryRow("SELECT EXISTS(SELECT 1 FROM link_operations WHERE result='pending') AND EXISTS(SELECT 1 FROM link_claims WHERE container=?)", e.Container).Scan(&pending); err != nil || !pending {
			t.Fatal(pending, err)
		}
		return link.Status{}, interrupted
	}
	if _, err := h.linkApply(context.Background(), c, "admin"); !errors.Is(err, interrupted) || guards != 1 {
		t.Fatal(err, guards)
	}
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='another'"); err == nil {
		t.Fatal("transferred a pending endpoint")
	}
	if _, err := h.db.SQL.Exec("DELETE FROM managed_containers"); err == nil {
		t.Fatal("released a pending endpoint")
	}
	recovered := NewHandler(h.db)
	recovered.run = d.run
	recovered.linkHelper = func(_ context.Context, r link.Request) (link.Status, error) {
		return link.Status{Protocol: link.Protocol, Applied: r.Config.Revision}, nil
	}
	recovered.restoreLinks(context.Background(), "", "", "startup")
	var applied int64
	if err := h.db.SQL.QueryRow("SELECT applied FROM link_local").Scan(&applied); err != nil || applied != 1 {
		t.Fatal(applied, err)
	}
	other := c
	other.Endpoints = nil
	if _, err := recovered.linkApply(context.Background(), other, "admin"); err == nil {
		t.Fatal("different payload accepted under same version")
	}
	other.Revision++
	if _, err := recovered.linkApply(context.Background(), other, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.linkApply(context.Background(), c, "admin"); err == nil {
		t.Fatal("stale grant restored")
	}
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='another'"); err != nil {
		t.Fatal("claim not released", err)
	}
}
