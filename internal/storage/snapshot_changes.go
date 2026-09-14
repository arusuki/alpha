package storage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (s *Service) Changes(id string, baseRevision int64) (object, error) {
	if patch, err := s.storedSnapshotChanges(id, baseRevision); err != sql.ErrNoRows {
		return patch, err
	}
	// A baseline has no directory updates yet.
	snapshot, err := s.Snapshot(id)
	if err != nil {
		return nil, err
	}
	// A first publication can race the initial lookup. Read its committed patch.
	if numberInt64(snapshot["revision"]) > 0 {
		return s.storedSnapshotChanges(id, baseRevision)
	}
	if baseRevision != 0 {
		return nil, httpapi.NewError(400, "扫描记录版本无效")
	}
	delete(snapshot, "tree")
	return object{"job_id": id, "base_revision": 0, "revision": 0, "replacements": []any{}, "ancestors": []any{}, "metadata": snapshot}, nil
}

func snapshotChangePaths(q platform.Queryer, id string, baseRevision int64) (map[string]bool, error) {
	rows, err := q.Query("SELECT path FROM snapshot_changes WHERE job_id=? AND revision>?", id, baseRevision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths[path] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A later parent expansion can remove a previously expanded child. Keep
	// the ancestor replacement, which already includes the final child state.
	for path := range paths {
		for parent := filepath.Dir(path); parent != path; parent = filepath.Dir(parent) {
			if paths[parent] {
				delete(paths, path)
				break
			}
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	return paths, nil
}

// Read only changed branches for SSE/polling. Unrelated nodes and private inode
// records never need to be loaded or serialized to produce a directory patch.
func (s *Service) storedSnapshotChanges(id string, baseRevision int64) (object, error) {
	tx, err := s.DB.SQL.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw, status string
	var revision int64
	if err := tx.QueryRow(`SELECT r.metadata,r.revision,j.status FROM snapshot_records r
 JOIN jobs j ON j.id=r.job_id WHERE r.job_id=?`, id).Scan(&raw, &revision, &status); err != nil {
		return nil, err
	}
	if status != "completed" {
		return nil, httpapi.NewError(409, "该任务尚未生成完整结果")
	}
	if baseRevision < 0 || baseRevision > revision {
		return nil, httpapi.NewError(400, "扫描记录版本无效")
	}
	paths, err := snapshotChangePaths(tx, id, baseRevision)
	if err != nil {
		return nil, err
	}
	replacements, ancestors := []any{}, []any{}
	seenAncestors := map[string]bool{}
	for path := range paths {
		entries, err := platform.Rows(tx, snapshotBranchCTE+` SELECT n.path,n.parent,n.value FROM snapshot_nodes n JOIN branch b ON n.path=b.path
 WHERE n.job_id=? ORDER BY n.parent,n.position`, id, path, id, id)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, httpapi.NewError(409, "目录变更与保存的结果不一致")
		}
		nodes := map[string]object{}
		for _, entry := range entries {
			var node object
			if err := json.Unmarshal([]byte(entry["value"].(string)), &node); err != nil {
				return nil, err
			}
			node["children"] = []any{}
			nodes[entry["path"].(string)] = node
		}
		var parent string
		for _, entry := range entries {
			p := entry["path"].(string)
			if p == path {
				parent = entry["parent"].(string)
				continue
			}
			n := nodes[entry["parent"].(string)]
			if n == nil {
				return nil, httpapi.NewError(503, "目录记录缺少父节点")
			}
			n["children"] = append(n["children"].([]any), nodes[p])
		}
		replacements = append(replacements, nodes[path])
		for parent != "" && !seenAncestors[parent] {
			seenAncestors[parent] = true
			var value, next string
			if err := tx.QueryRow("SELECT value,parent FROM snapshot_nodes WHERE job_id=? AND path=?", id, parent).Scan(&value, &next); err != nil {
				if err == sql.ErrNoRows {
					return nil, httpapi.NewError(503, "目录记录缺少祖先节点")
				}
				return nil, err
			}
			var node object
			if err := json.Unmarshal([]byte(value), &node); err != nil {
				return nil, err
			}
			delete(node, "children")
			ancestors = append(ancestors, node)
			parent = next
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var metadata object
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil, err
	}
	delete(metadata, "tree")
	metadata, err = s.publicSnapshot(metadata, id, revision)
	if err != nil {
		return nil, err
	}
	return object{"job_id": id, "base_revision": baseRevision, "revision": revision, "replacements": replacements, "ancestors": ancestors, "metadata": metadata}, nil
}
