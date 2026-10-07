package containers

import (
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

// Only managed containers are offered for adoption, even when Docker is unavailable.
func (h *Handler) candidates() (any, error) {
	rows, err := platform.Rows(h.db.SQL, `SELECT c.id,c.name,c.owner,COALESCE(s.member_id,'') AS member_id,COALESCE(s.username,'') AS claimed_by
 FROM managed_containers c LEFT JOIN member_container_slots s ON s.container_id=c.id AND s.deleted=0 ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	out := []candidate{}
	for _, r := range rows {
		c := candidate{ID: r["id"].(string), Name: r["name"].(string), Owner: r["owner"].(string), MemberID: r["member_id"].(string), ClaimedBy: r["claimed_by"].(string)}
		out = append(out, c)
	}
	return map[string]any{"containers": out}, nil
}
