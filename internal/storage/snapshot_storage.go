package storage

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
)

func validateSnapshotDocker(docker object) error {
	if len(docker) == 0 {
		return nil
	}
	endpoint, endpointOK := docker["endpoint"].(string)
	id, idOK := docker["id"].(string)
	root, rootOK := docker["root"].(string)
	canonical, canonicalOK := docker["root_canonical"].(string)
	if !endpointOK || !idOK || id == "" || !rootOK || !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		!canonicalOK || !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
		return fmt.Errorf("扫描记录中的 Docker 身份或数据目录无效，请使用新的数据目录重新扫描")
	}
	if _, err := dockerEndpointSocket(endpoint); err != nil {
		return fmt.Errorf("扫描记录中的 Docker endpoint 无效，请使用新的数据目录重新扫描: %w", err)
	}
	return nil
}

// Keep the recursive frontier outside the node lookup. An ordinary JOIN lets
// SQLite scan every node in the record for each descendant, holding up both
// publication and changes reads (and cancellation waiting for their locks).
const snapshotBranchCTE = `WITH RECURSIVE branch(path) AS (
 SELECT path FROM snapshot_nodes WHERE job_id=? AND path=?
 UNION ALL SELECT n.path FROM branch b CROSS JOIN snapshot_nodes n WHERE n.job_id=? AND n.parent=b.path
 )`

// Children are stored separately; changing a child does not rewrite siblings or
// a directory row whose own counters stayed the same. A fresh scan can also
// produce equal values at new addresses, which is common when refreshing.
func sameStoredNode(a, b *Node) bool {
	if a == b {
		return true
	}
	return a != nil && b != nil && a.Scanning == b.Scanning && a.SizeUnknown == b.SizeUnknown &&
		a.Name == b.Name && a.Path == b.Path && a.Kind == b.Kind &&
		a.Allocated == b.Allocated && a.Apparent == b.Apparent && a.Files == b.Files && a.Errors == b.Errors &&
		a.Omitted == b.Omitted && a.Reference == b.Reference && a.Reason == b.Reason &&
		a.Excluded == b.Excluded && a.PermissionDenied == b.PermissionDenied && a.OmittedReferences == b.OmittedReferences &&
		slices.Equal(a.OmittedReferenceTargets, b.OmittedReferenceTargets) &&
		(a.DeviceAllocated == nil) == (b.DeviceAllocated == nil) && maps.Equal(a.DeviceAllocated, b.DeviceAllocated)
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
		return nil, fmt.Errorf("不支持的扫描结果版本：%d，需要版本 %d；请使用新的数据目录重新扫描", result.SchemaVersion, snapshotVersion)
	}
	if err := validateSnapshotDocker(result.Docker); err != nil {
		return nil, err
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
	if err := validateIncrementalNodes(nodes); err != nil {
		return nil, err
	}
	return &result, nil
}
