package bastion

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestMultipleKeyRotationRetriesRevocationAndDeletion(t *testing.T) {
	h, _, m := fixture(t)
	h.KeyEditor = h.editMemberKey
	ctx := context.Background()
	pool := map[string]bool{}
	failRemove := true
	h.RemoteCommand = func(_ context.Context, _ ShareNode, req commandRequest) (commandReply, error) {
		switch req.Operation {
		case "ensure":
			for _, key := range req.Keys {
				pool[key] = true
			}
		case "remove":
			if failRemove {
				return commandReply{}, errors.New("offline")
			}
			delete(pool, req.Key)
		}
		return commandReply{}, nil
	}
	queue := func(*sql.Tx) error { return nil }
	both := testKey + "\n" + managerTestKey
	if err := h.ChangeMemberKeys(m.ID, both, queue); err != nil {
		t.Fatal(err)
	}
	if err := h.Apply(ctx, m.ID, both, false); err != nil || len(pool) != 2 {
		t.Fatalf("initial: %v %v", pool, err)
	}
	if err := h.ChangeMemberKeys(m.ID, managerTestKey, queue); err != nil {
		t.Fatal(err)
	}
	if err := h.Apply(ctx, m.ID, managerTestKey, false); err == nil {
		t.Fatal("ignored failed revoke")
	}
	var pending int
	if err := h.DB.SQL.QueryRow("SELECT count(*) FROM member_key_revocations WHERE member_id=?", m.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("lost pending revoke: %d %v", pending, err)
	}
	// Another edit before retry must retain the earlier revoke journal.
	if err := h.ChangeMemberKeys(m.ID, managerTestKey, queue); err != nil {
		t.Fatal(err)
	}
	failRemove = false
	if err := h.SyncKeys(ctx); err != nil || len(pool) != 1 || !pool[managerTestKey] {
		t.Fatalf("retry: %v %v", pool, err)
	}
	if _, err := h.DB.SQL.Exec("UPDATE members SET status='deleting' WHERE id=?", m.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.Apply(ctx, m.ID, managerTestKey, true); err != nil || len(pool) != 0 {
		t.Fatalf("delete: %v %v", pool, err)
	}
}
