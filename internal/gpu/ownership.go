package gpu

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"project-alpha/internal/containerorigin"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type RootlessOwnership struct {
	ContainerID      string  `json:"container_id"`
	OwnerContainerID string  `json:"owner_container_id"`
	Owner            string  `json:"owner"`
	UpdatedAt        float64 `json:"updated_at"`
	Actor            string  `json:"actor"`
}

func RootlessOwnerships(db *platform.Database) ([]RootlessOwnership, error) {
	rows, err := db.SQL.Query(`SELECT r.container_id,r.owner_container_id,o.owner,r.updated_at,r.actor
 FROM rootless_container_owners r JOIN owners o ON o.container_id=r.owner_container_id ORDER BY r.container_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RootlessOwnership{}
	for rows.Next() {
		var row RootlessOwnership
		if err := rows.Scan(&row.ContainerID, &row.OwnerContainerID, &row.Owner, &row.UpdatedAt, &row.Actor); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RootlessContainers verifies the configured engine rather than trusting names,
// labels, or a caller-supplied socket. Stopped containers can also be assigned.
func RootlessContainers(ctx context.Context, db *platform.Database) ([]string, error) {
	socket, err := platform.RootlessControlSocket(db.SQL)
	if err != nil {
		return nil, err
	}
	info, err := command(ctx, "docker", "--host", "unix://"+socket, "info", "--format", "{{json .SecurityOptions}}")
	if err != nil {
		return nil, fmt.Errorf("无法验证 rootless Docker 引擎: %w", err)
	}
	var security []string
	if err := json.Unmarshal(info, &security); err != nil || !slices.Contains(security, "name=rootless") {
		return nil, fmt.Errorf("配置的 Docker 引擎不是 rootless，拒绝登记归属")
	}
	raw, err := command(ctx, "docker", "--host", "unix://"+socket, "ps", "-a", "--no-trunc", "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("无法读取 rootless Docker 容器: %w", err)
	}
	ids := strings.Fields(string(raw))
	for _, id := range ids {
		if !containerorigin.ValidID(id) {
			return nil, fmt.Errorf("rootless Docker 返回无效容器 ID")
		}
	}
	return ids, nil
}

// SetRootlessOwners records an administrator's explicit attribution separately
// from observed creation provenance. Empty owner removes the manual binding;
// historical GPU intervals and the primary container's one-to-one slot stay put.
func SetRootlessOwners(ctx context.Context, db *platform.Database, ids []string, owner, actor string, dryRun bool) error {
	owner = strings.TrimSpace(owner)
	if len(ids) == 0 || len(ids) > 1000 {
		return httpapi.NewError(400, "每次需提供 1–1000 个 rootless 容器完整 ID")
	}
	for _, id := range ids {
		if !containerorigin.ValidID(id) {
			return httpapi.NewError(400, "无效的 rootless 容器完整 ID")
		}
	}
	if owner != "" {
		available, err := RootlessContainers(ctx, db)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if !slices.Contains(available, id) {
				return httpapi.NewError(409, "容器不在当前 rootless Docker 引擎中: "+id)
			}
		}
	}
	return db.Transaction(func(tx *sql.Tx) error {
		parent := ""
		if owner != "" {
			err := tx.QueryRow("SELECT container_id FROM owners WHERE owner=?", owner).Scan(&parent)
			if err == sql.ErrNoRows {
				return httpapi.NewError(400, "该使用者在本节点没有已归属的主容器")
			}
			if err != nil {
				return err
			}
			for _, id := range ids {
				var primary bool
				if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM owners WHERE container_id=? UNION ALL SELECT 1 FROM managed_containers WHERE id=?)", id, id).Scan(&primary); err != nil {
					return err
				}
				if primary {
					return httpapi.NewError(409, "主容器不能登记为 rootless 子容器: "+id)
				}
			}
		}
		if dryRun {
			return nil
		}
		for _, id := range ids {
			if owner == "" {
				if _, err := tx.Exec("DELETE FROM rootless_container_owners WHERE container_id=?", id); err != nil {
					return err
				}
			} else if _, err := tx.Exec(`INSERT INTO rootless_container_owners(container_id,owner_container_id,updated_at,actor) VALUES(?,?,?,?)
 ON CONFLICT(container_id) DO UPDATE SET owner_container_id=excluded.owner_container_id,updated_at=excluded.updated_at,actor=excluded.actor`, id, parent, platform.Now(), actor); err != nil {
				return err
			}
		}
		detail, err := json.Marshal(map[string]any{"container_ids": ids, "owner": owner, "owner_container_id": parent})
		if err != nil {
			return err
		}
		return platform.Audit(tx, actor, "rootless.owner", string(detail))
	})
}

func (m *Monitor) rootlessOwners(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if r.Method == http.MethodGet {
		rows, err := RootlessOwnerships(m.db)
		return 200, map[string]any{"owners": rows}, err
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return 0, nil, httpapi.NewError(405, "不支持的归属操作")
	}
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	var request struct {
		ContainerIDs []string `json:"container_ids"`
		Owner        string   `json:"owner"`
	}
	if err := httpapi.DecodeBody(w, r, &request); err != nil {
		return 0, nil, err
	}
	if r.Method == http.MethodDelete {
		request.Owner = ""
	} else if strings.TrimSpace(request.Owner) == "" {
		return 0, nil, httpapi.NewError(400, "归属使用者不能为空；撤销手动归属请使用 DELETE")
	}
	if err := SetRootlessOwners(r.Context(), m.db, request.ContainerIDs, request.Owner, user.Username, false); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"ok": true}, nil
}
