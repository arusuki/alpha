package app

import (
	"database/sql"
	"encoding/json"
	"os"
	"sort"

	"project-alpha/internal/cluster"
	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

// inventory reads only small persisted metadata, never the snapshot file tree.
// A single transaction keeps ownership and managed records at the same revision.
func inventory(db *platform.Database) (cluster.Inventory, error) {
	out := cluster.Inventory{Containers: []cluster.Container{}}
	out.Host, _ = os.Hostname()
	tx, err := db.SQL.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	byID := map[string]cluster.Container{}
	var raw string
	err = tx.QueryRow(`SELECT r.job_id,r.metadata FROM snapshot_records r JOIN jobs j ON j.id=r.job_id
 WHERE j.status='completed' AND j.trigger<>'incremental' ORDER BY j.finished_at DESC LIMIT 1`).Scan(&out.SnapshotID, &raw)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil {
		var snapshot struct {
			Containers []storage.Container `json:"containers"`
			FinishedAt string              `json:"finished_at"`
		}
		if err = json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return out, err
		}
		out.ObservedAt = snapshot.FinishedAt
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
