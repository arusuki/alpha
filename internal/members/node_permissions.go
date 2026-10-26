package members

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"project-alpha/internal/httpapi"
)

// AllNodes includes future workers. An empty explicit list grants no nodes.
type NodePermissions struct {
	AllNodes bool     `json:"all_nodes"`
	NodeIDs  []string `json:"node_ids"`
}

func (p NodePermissions) Allows(nodeID string) bool {
	return p.AllNodes || slices.Contains(p.NodeIDs, nodeID)
}

func (p NodePermissions) Validate() error {
	if p.NodeIDs == nil || len(p.NodeIDs) > 128 || p.AllNodes && len(p.NodeIDs) != 0 {
		return httpapi.NewError(400, "请选择全部节点或指定节点；node_ids 必须是数组，最多 128 项")
	}
	seen := map[string]bool{}
	for _, id := range p.NodeIDs {
		if !invitationID.MatchString(id) || seen[id] {
			return httpapi.NewError(400, "授权节点标识无效或重复")
		}
		seen[id] = true
	}
	return nil
}

func (p NodePermissions) ValidateNodes(tx *sql.Tx) error {
	if err := p.Validate(); err != nil {
		return err
	}
	for _, id := range p.NodeIDs {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM cluster_nodes WHERE id=? AND kind='worker')", id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return httpapi.NewError(400, "授权节点不存在或不是 worker，请刷新节点列表")
		}
	}
	return nil
}

func ReadNodePermissions(q queryRower, memberID string) (NodePermissions, error) {
	var raw string
	if err := q.QueryRow("SELECT permissions FROM member_node_permissions WHERE member_id=?", memberID).Scan(&raw); err != nil {
		return NodePermissions{}, fmt.Errorf("使用者节点权限不可用: %w", err)
	}
	return parseNodePermissions(raw)
}

func parseNodePermissions(raw string) (NodePermissions, error) {
	var p NodePermissions
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("节点权限格式无效: %w", err)
	}
	return p, p.Validate()
}

func (s *Store) InvitationPermissions(code string) (NodePermissions, error) {
	if err := s.CheckInvitation(code); err != nil {
		return NodePermissions{}, err
	}
	var raw string
	err := s.SQL.QueryRow(`SELECT p.permissions FROM invitation_node_permissions p JOIN member_invitations i ON i.id=p.invitation_id WHERE i.code_hash=? AND i.revoked=0 AND i.used<i.quota`, hashCode(code)).Scan(&raw)
	if err != nil {
		return NodePermissions{}, httpapi.NewError(400, "邀请码或节点权限无效，请重新获取邀请")
	}
	return parseNodePermissions(raw)
}
