package cluster

import (
	"database/sql"
	"net/http"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func (h *Control) requireMemberNode(memberID, nodeID string) error {
	p, err := members.ReadNodePermissions(h.DB.SQL, memberID)
	if err != nil {
		return err
	}
	if !p.Allows(nodeID) {
		return httpapi.NewError(403, "没有在此节点拥有容器的权限，请联系管理员")
	}
	return nil
}

func (h *Control) memberNodePermissions(w http.ResponseWriter, r *http.Request, id, actor string) (int, any, error) {
	if r.Method == "GET" {
		p, err := members.ReadNodePermissions(h.DB.SQL, id)
		return 200, p, err
	}
	if r.Method != "PUT" {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	var p members.NodePermissions
	if err := httpapi.DecodeBody(w, r, &p); err != nil {
		return 0, nil, err
	}
	gate := h.memberGate(id)
	if !gate.TryLock() {
		return 0, nil, httpapi.NewError(409, "资源分配正在执行，请稍后修改权限")
	}
	defer gate.Unlock()
	err := h.DB.Transaction(func(tx *sql.Tx) error {
		if err := p.ValidateNodes(tx); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow("SELECT status FROM members WHERE id=?", id).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return httpapi.NewError(404, "使用者不存在")
			}
			return err
		}
		if status != "active" {
			return httpapi.NewError(409, "使用者正在删除，不能修改权限")
		}
		result, err := tx.Exec("UPDATE member_node_permissions SET permissions=? WHERE member_id=?", httpapi.JSONText(p), id)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return httpapi.NewError(409, "使用者节点权限缺失，请检查数据库升级")
		}
		return platform.Audit(tx, actor, "member.node_permissions", id+" / "+httpapi.JSONText(p))
	})
	return 200, p, err
}
