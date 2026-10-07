package containers

import (
	"context"
	"database/sql"
	"strings"

	"project-alpha/internal/platform"
)

type candidate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	MemberID  string `json:"member_id,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
}

func inspectionOwner(c inspection) string {
	if owner, ok := c.Config.Labels["project-alpha.owner"]; ok {
		return owner
	}
	return strings.TrimPrefix(c.Name, "/")
}

// Include all live Docker identities and retain managed records even if Docker
// is unavailable. The adoption operation performs the full template checks.
func (h *Handler) candidates(ctx context.Context, cfg Config) (any, error) {
	rows, err := platform.Rows(h.db.SQL, `SELECT c.id,c.name,c.owner,COALESCE(s.member_id,'') AS member_id,COALESCE(s.username,'') AS claimed_by
 FROM managed_containers c LEFT JOIN member_container_slots s ON s.container_id=c.id AND s.deleted=0 ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	out := []candidate{}
	seen := map[string]bool{}
	for _, r := range rows {
		c := candidate{ID: r["id"].(string), Name: r["name"].(string), Owner: r["owner"].(string), MemberID: r["member_id"].(string), ClaimedBy: r["claimed_by"].(string)}
		out = append(out, c)
		seen[c.ID] = true
	}
	result := func(err error) any {
		v := map[string]any{"containers": out}
		if err != nil {
			v["error"] = err.Error()
		}
		return v
	}
	refs, err := h.run(ctx, cfg.Endpoint, []string{"ps", "-aq", "--no-trunc"}, "")
	if err != nil {
		return result(err), nil
	}
	for _, id := range strings.Fields(refs) {
		if seen[id] {
			continue
		}
		c, err := h.inspect(ctx, cfg.Endpoint, id)
		if err != nil {
			return result(err), nil
		}
		owner := inspectionOwner(c)
		if h.ReadOwner != nil {
			owner, err = h.ReadOwner(c.ID, owner)
			if err != nil {
				return result(err), nil
			}
		}
		var memberID, claimedBy string
		err = h.db.SQL.QueryRow("SELECT member_id,username FROM member_container_slots WHERE container_id=? AND deleted=0", c.ID).Scan(&memberID, &claimedBy)
		if err != nil && err != sql.ErrNoRows {
			return result(err), nil
		}
		out = append(out, candidate{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Owner: owner, MemberID: memberID, ClaimedBy: claimedBy})
	}
	return result(nil), nil
}
