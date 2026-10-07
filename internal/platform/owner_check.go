package platform

import (
	"database/sql"
	"project-alpha/internal/httpapi"
)

// CheckContainerOwner includes the latest scanned containers that have not yet
// been imported. Explicit overlays override Docker labels, including empty ones.
func CheckContainerOwner(tx *sql.Tx, id, owner string) error {
	if owner == "" {
		return nil
	}
	var conflict bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM owners WHERE owner=? AND container_id<>?)`, owner, id).Scan(&conflict)
	if err != nil {
		return err
	}
	if !conflict {
		err = tx.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM json_each((SELECT r.metadata FROM snapshot_records r JOIN jobs j ON j.id=r.job_id ORDER BY j.created_at DESC,j.id DESC LIMIT 1),'$.containers') c
 LEFT JOIN owners o ON o.container_id=json_extract(c.value,'$.id')
 WHERE json_extract(c.value,'$.id')<>? AND COALESCE(o.owner,json_extract(c.value,'$.owner'))=?)`, id, owner).Scan(&conflict)
		if err != nil {
			return err
		}
	}
	if conflict {
		return httpapi.NewError(409, "该使用者在此 node 已有容器；每个用户每个节点最多一个容器")
	}
	return nil
}
