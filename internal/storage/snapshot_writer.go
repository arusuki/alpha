package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

type snapshotNodeState struct {
	node     *Node
	parent   string
	position int
	identity InodeRecord
}

// This cache belongs to one directory worker and advances only after commit.
// It never caches filesystem measurements across independent scans.
type snapshotWriter struct {
	initialized bool
	previous    *Snapshot
	nodes       map[string]snapshotNodeState
	identities  map[string]InodeRecord
}

func inodeIndex(records []InodeRecord) map[string]InodeRecord {
	result := make(map[string]InodeRecord, len(records))
	for _, r := range records {
		result[r.Path] = r
	}
	return result
}

func newSnapshotWriter(previous *Snapshot, initialized bool) *snapshotWriter {
	w := &snapshotWriter{initialized: initialized, previous: previous, nodes: map[string]snapshotNodeState{}}
	if previous == nil {
		w.identities = map[string]InodeRecord{}
		return w
	}
	w.identities = inodeIndex(previous.Accounting)
	if !initialized {
		return w
	}
	capacity := int(min(max(numberInt64(previous.Scan["retained_nodes"])+1, int64(len(previous.Accounting))), int64(maxSnapshotNodes)))
	w.nodes = make(map[string]snapshotNodeState, capacity)
	var visit func(*Node, string, int)
	visit = func(n *Node, parent string, position int) {
		w.nodes[n.Path] = snapshotNodeState{n, parent, position, w.identities[n.Path]}
		for i, child := range n.Children {
			visit(child, n.Path, i)
		}
	}
	visit(previous.Tree, "", 0)
	return w
}

type snapshotWriteRow struct {
	state    snapshotNodeState
	value    string
	identity any
}

type snapshotWrite struct {
	result     *Snapshot
	metadata   string
	rows       []snapshotWriteRow
	nodes      map[string]snapshotNodeState
	removed    []string
	identities map[string]InodeRecord
}

// Diff and JSON work happens before BEGIN IMMEDIATE, so readers, progress and
// cancellation do not wait for CPU work under SQLite's writer lock.
func (w *snapshotWriter) prepare(ctx context.Context, path string, result *Snapshot) (*snapshotWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identitiesUnchanged := w.previous != nil && len(result.Accounting) == len(w.previous.Accounting) &&
		(len(result.Accounting) == 0 || &result.Accounting[0] == &w.previous.Accounting[0])
	identities := w.identities
	if !identitiesUnchanged {
		identities = inodeIndex(result.Accounting)
	}
	write := &snapshotWrite{result: result, nodes: map[string]snapshotNodeState{}, identities: identities}
	skipped := map[string]bool{}
	var visit func(*Node, string, int, bool) error
	visit = func(n *Node, parent string, position int, branch bool) error {
		branch = branch || n.Path == path
		if !branch && n.Path != result.Tree.Path && !within(path, n.Path) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		state := snapshotNodeState{n, parent, position, identities[n.Path]}
		old := w.nodes[n.Path]
		sameLocation := old.parent == parent && old.position == position
		if w.initialized && branch && identitiesUnchanged && old.node == n && sameLocation {
			skipped[n.Path] = true
			return nil
		}
		write.nodes[n.Path] = state
		if !w.initialized || !sameLocation || old.identity != state.identity || !sameStoredNode(old.node, n) {
			write.rows = append(write.rows, snapshotWriteRow{state: state})
		}
		for i, child := range n.Children {
			if err := visit(child, n.Path, i, branch); err != nil {
				return err
			}
		}
		return nil
	}
	if w.initialized && w.nodes[path].node == nil {
		return nil, fmt.Errorf("目录记录不存在：%s", path)
	}
	if err := visit(result.Tree, "", 0, !w.initialized); err != nil {
		return nil, err
	}
	if w.initialized {
		var removed func(*Node)
		removed = func(n *Node) {
			if skipped[n.Path] {
				return
			}
			if _, ok := write.nodes[n.Path]; !ok {
				write.removed = append(write.removed, n.Path)
			}
			for _, child := range n.Children {
				removed(child)
			}
		}
		removed(w.nodes[path].node)
	}
	metadata := *result
	metadata.Tree, metadata.Accounting = nil, nil
	raw, err := json.Marshal(&metadata)
	if err != nil {
		return nil, err
	}
	write.metadata = string(raw)
	// Encode independent rows in bounded parallel chunks, never parallel SQL
	// writers. Small checkpoints stay on this goroutine to avoid startup cost.
	workers := 1
	if len(write.rows) >= 512 {
		workers = min(4, runtime.GOMAXPROCS(0))
	}
	var wg sync.WaitGroup
	errors := make([]error, workers)
	encode := func(worker int) {
		for i := worker; i < len(write.rows); i += workers {
			if err := ctx.Err(); err != nil {
				errors[worker] = err
				return
			}
			row := &write.rows[i]
			value := *row.state.node
			value.Children = nil
			raw, err := json.Marshal(&value)
			if err != nil {
				errors[worker] = err
				return
			}
			row.value = string(raw)
			if row.state.identity.Path != "" {
				raw, _ := json.Marshal(row.state.identity)
				row.identity = string(raw)
			}
		}
	}
	for worker := 1; worker < workers; worker++ {
		wg.Go(func() { encode(worker) })
	}
	encode(0)
	wg.Wait()
	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}
	return write, ctx.Err()
}

const snapshotWriteBatch = 128 // 768 bind parameters, below SQLite's 999 minimum.

func (write *snapshotWrite) store(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_records(job_id,revision,root_path,metadata) VALUES(?,?,?,?)
 ON CONFLICT(job_id) DO UPDATE SET revision=excluded.revision,metadata=excluded.metadata`, id, write.result.Revision, write.result.Tree.Path, write.metadata); err != nil {
		return err
	}
	const insert = `INSERT INTO snapshot_nodes(job_id,path,parent,position,value,identity) VALUES `
	const conflict = ` ON CONFLICT(job_id,path) DO UPDATE SET parent=excluded.parent,position=excluded.position,value=excluded.value,identity=excluded.identity`
	queryFor := func(count int) string {
		return insert + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?),", count), ",") + conflict
	}
	var batch *sql.Stmt
	if len(write.rows) >= snapshotWriteBatch {
		var err error
		batch, err = tx.PrepareContext(ctx, queryFor(snapshotWriteBatch))
		if err != nil {
			return err
		}
		defer batch.Close()
	}
	for start := 0; start < len(write.rows); start += snapshotWriteBatch {
		rows := write.rows[start:min(start+snapshotWriteBatch, len(write.rows))]
		args := make([]any, 0, 6*len(rows))
		for _, row := range rows {
			args = append(args, id, row.state.node.Path, row.state.parent, row.state.position, row.value, row.identity)
		}
		var err error
		if len(rows) == snapshotWriteBatch {
			_, err = batch.ExecContext(ctx, args...)
		} else {
			_, err = tx.ExecContext(ctx, queryFor(len(rows)), args...)
		}
		if err != nil {
			return err
		}
	}
	for start := 0; start < len(write.removed); start += snapshotWriteBatch {
		paths := write.removed[start:min(start+snapshotWriteBatch, len(write.removed))]
		args := make([]any, 1, len(paths)+1)
		args[0] = id
		for _, path := range paths {
			args = append(args, path)
		}
		query := "DELETE FROM snapshot_nodes WHERE job_id=? AND path IN (" + strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",") + ")"
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (w *snapshotWriter) committed(write *snapshotWrite) {
	for _, path := range write.removed {
		delete(w.nodes, path)
	}
	for path, state := range write.nodes {
		w.nodes[path] = state
	}
	w.previous, w.identities, w.initialized = write.result, write.identities, true
}
