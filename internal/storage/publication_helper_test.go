package storage

import (
	"context"
	"database/sql"
)

// Fixture helpers for one-off writes. Production reuses one publisher and its
// committed index for the entire worker instead of reconstructing it per call.
func (d *Store) publishDirectory(ctx context.Context, id string, plan scanPlan, result *Snapshot, progress object, complete bool, previous *Snapshot) error {
	p, err := d.newDirectoryPublisher(id, plan, previous)
	if err != nil {
		return err
	}
	return p.publish(ctx, result, progress, complete)
}

func storeSnapshot(tx *sql.Tx, id, path string, result, previous *Snapshot) error {
	var initialized bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM snapshot_records WHERE job_id=?)", id).Scan(&initialized); err != nil {
		return err
	}
	write, err := newSnapshotWriter(previous, initialized).prepare(context.Background(), path, result)
	if err != nil {
		return err
	}
	return write.store(context.Background(), tx, id)
}
