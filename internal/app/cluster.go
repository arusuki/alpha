package app

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"project-alpha/internal/cluster"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

// inventory decodes only summary fields, without materializing the snapshot tree.
// A single transaction keeps ownership and managed records at the same revision.
func inventory(db *platform.Database) (cluster.Inventory, error) {
	out := cluster.Inventory{Containers: []cluster.Container{}, Filesystems: []cluster.Filesystem{}}
	out.Host, _ = os.Hostname()
	tx, err := db.SQL.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	byID := map[string]cluster.Container{}
	var raw sql.NullString
	err = tx.QueryRow(`SELECT j.id,r.metadata FROM jobs j LEFT JOIN snapshot_records r ON r.job_id=j.id
 WHERE j.status='completed' AND j.trigger<>'incremental'
 ORDER BY j.finished_at DESC,j.created_at DESC,j.id DESC LIMIT 1`).Scan(&out.SnapshotID, &raw)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil {
		data := []byte(raw.String)
		if !raw.Valid {
			// Full scans persist an immutable file; the indexed record exists
			// only after directory exploration publishes its first revision.
			data, err = os.ReadFile(filepath.Join(db.Directory, "results", out.SnapshotID, "snapshot.json"))
			if err != nil {
				return out, httpapi.NewError(503, "该扫描结果文件无法读取")
			}
		}
		var snapshot struct {
			SchemaVersion int                  `json:"schema_version"`
			Tree          *struct{}            `json:"tree"`
			Containers    []storage.Container  `json:"containers"`
			FinishedAt    string               `json:"finished_at"`
			Filesystems   []cluster.Filesystem `json:"filesystems"`
		}
		if err = json.Unmarshal(data, &snapshot); err != nil || !raw.Valid && snapshot.Tree == nil {
			return out, httpapi.NewError(503, "该扫描结果文件无法读取")
		}
		if err = storage.ValidateSnapshotVersion(snapshot.SchemaVersion); err != nil {
			return out, httpapi.NewError(503, err.Error())
		}
		out.ObservedAt = snapshot.FinishedAt
		out.Filesystems = append(out.Filesystems, snapshot.Filesystems...)
		for _, c := range snapshot.Containers {
			byID[c.ID] = cluster.Container{ID: c.ID, Name: c.Name, Owner: c.Owner, State: c.State, ObservedAt: snapshot.FinishedAt}
		}
	}
	rows, err := platform.Rows(tx, "SELECT id,name,owner FROM managed_containers")
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		id := r["id"].(string)
		c := byID[id]
		c.ID = id
		c.Name = r["name"].(string)
		c.Owner = r["owner"].(string)
		c.Managed = true
		byID[id] = c
	}
	owners, err := platform.Rows(tx, "SELECT container_id,owner FROM owners")
	if err != nil {
		return out, err
	}
	for _, r := range owners {
		if c, ok := byID[r["container_id"].(string)]; ok {
			c.Owner = r["owner"].(string)
			byID[c.ID] = c
		}
	}
	jobs, err := platform.Rows(tx, "SELECT id,status,progress FROM jobs WHERE status IN ('queued','running','cancelling') LIMIT 1")
	if err != nil {
		return out, err
	}
	if len(jobs) > 0 {
		out.Active = jobs[0]
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	for _, c := range byID {
		out.Containers = append(out.Containers, c)
	}
	sort.Slice(out.Containers, func(i, j int) bool {
		a, b := out.Containers[i], out.Containers[j]
		if a.Name == b.Name {
			return a.ID < b.ID
		}
		return a.Name < b.Name
	})
	return out, nil
}
