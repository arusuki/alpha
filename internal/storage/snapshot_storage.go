package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"project-alpha/internal/httpapi"
)

// Keep the recursive frontier outside the node lookup. An ordinary JOIN lets
// SQLite scan every node in the record for each descendant, holding up both
// publication and changes reads (and cancellation waiting for their locks).
const snapshotBranchCTE = `WITH RECURSIVE branch(path) AS (
 SELECT path FROM snapshot_nodes WHERE job_id=? AND path=?
 UNION ALL SELECT n.path FROM branch b CROSS JOIN snapshot_nodes n WHERE n.job_id=? AND n.parent=b.path
 )`

// Update current nodes and ancestor totals atomically with the public revision.
func storeSnapshot(tx *sql.Tx, id, path string, result *Snapshot) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM snapshot_records WHERE job_id=?", id).Scan(&exists); err != nil {
		return err
	}
	metadata := *result
	metadata.Tree = nil
	identities := map[string]InodeRecord{}
	for _, record := range result.Accounting {
		identities[record.Path] = record
	}
	// Identity records belong to their node, so unchanged directories never
	// cause their entire inode ledger to be serialized and written again.
	metadata.Accounting = nil
	if _, err := tx.Exec(`INSERT INTO snapshot_records(job_id,revision,root_path,metadata) VALUES(?,?,?,?)
 ON CONFLICT(job_id) DO UPDATE SET revision=excluded.revision,metadata=excluded.metadata`, id, result.Revision, result.Tree.Path, httpapi.JSONText(&metadata)); err != nil {
		return err
	}
	write, err := tx.Prepare(`INSERT INTO snapshot_nodes(job_id,path,parent,position,value,identity) VALUES(?,?,?,?,?,?)
 ON CONFLICT(job_id,path) DO UPDATE SET parent=excluded.parent,position=excluded.position,value=excluded.value,identity=excluded.identity
 WHERE parent<>excluded.parent OR position<>excluded.position OR value<>excluded.value OR identity IS NOT excluded.identity`)
	if err != nil {
		return err
	}
	defer write.Close()
	writeNode := func(n *Node, parent string, position int) error {
		value := *n
		value.Children = nil
		var identity any
		if record, ok := identities[n.Path]; ok {
			identity = httpapi.JSONText(record)
		}
		_, err := write.Exec(id, n.Path, parent, position, httpapi.JSONText(&value), identity)
		return err
	}
	old := map[string]bool{}
	if exists != 0 {
		rs, err := tx.Query(snapshotBranchCTE+` SELECT path FROM branch`, id, path, id)
		if err != nil {
			return err
		}
		for rs.Next() {
			var p string
			if err := rs.Scan(&p); err != nil {
				rs.Close()
				return err
			}
			old[p] = true
		}
		err = rs.Err()
		rs.Close()
		if err != nil {
			return err
		}
		if !old[path] {
			return fmt.Errorf("目录记录不存在：%s", path)
		}
	}
	var visit func(*Node, string, int, bool) error
	visit = func(n *Node, parent string, position int, branch bool) error {
		branch = branch || n.Path == path
		if !branch && n.Path != result.Tree.Path && !within(path, n.Path) {
			return nil
		}
		if err := writeNode(n, parent, position); err != nil {
			return err
		}
		delete(old, n.Path)
		for i, child := range n.Children {
			if err := visit(child, n.Path, i, branch); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(result.Tree, "", 0, exists == 0); err != nil {
		return err
	}
	for p := range old {
		if _, err := tx.Exec("DELETE FROM snapshot_nodes WHERE job_id=? AND path=?", id, p); err != nil {
			return err
		}
	}
	return nil
}

func (d *Store) storedSnapshot(id string) (*Snapshot, error) {
	// The immediate transaction keeps all rows at one revision. Decode and link
	// after committing to release the database lock promptly.
	tx, err := d.SQL.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var metadata, root string
	var revision int64
	if err := tx.QueryRow("SELECT metadata,root_path,revision FROM snapshot_records WHERE job_id=?", id).Scan(&metadata, &root, &revision); err != nil {
		return nil, err
	}
	rs, err := tx.Query("SELECT path,parent,value,coalesce(identity,'') FROM snapshot_nodes WHERE job_id=? ORDER BY parent,position", id)
	if err != nil {
		return nil, err
	}
	type row struct{ path, parent, value, identity string }
	values := []row{}
	for rs.Next() {
		var v row
		if err := rs.Scan(&v.path, &v.parent, &v.value, &v.identity); err != nil {
			rs.Close()
			return nil, err
		}
		values = append(values, v)
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var result Snapshot
	if err := json.Unmarshal([]byte(metadata), &result); err != nil {
		return nil, err
	}
	if result.SchemaVersion != snapshotVersion {
		return nil, fmt.Errorf("不支持的扫描结果版本：%d", result.SchemaVersion)
	}
	nodes := map[string]*Node{}
	for _, v := range values {
		var n Node
		if err := json.Unmarshal([]byte(v.value), &n); err != nil {
			return nil, err
		}
		n.Children = []*Node{}
		nodes[v.path] = &n
		if v.identity != "" {
			var record InodeRecord
			if err := json.Unmarshal([]byte(v.identity), &record); err != nil {
				return nil, err
			}
			result.Accounting = append(result.Accounting, record)
		}
	}
	for _, v := range values {
		if v.path != root {
			parent := nodes[v.parent]
			if parent == nil {
				return nil, fmt.Errorf("目录记录缺少父节点：%s", v.path)
			}
			parent.Children = append(parent.Children, nodes[v.path])
		}
	}
	result.Tree, result.JobID, result.Revision = nodes[root], id, revision
	if result.Tree == nil {
		return nil, fmt.Errorf("扫描记录缺少根节点")
	}
	return &result, nil
}
