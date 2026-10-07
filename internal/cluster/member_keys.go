package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func (h *Control) changeMemberKeys(w http.ResponseWriter, r *http.Request, id string) (int, any, error) {
	var req struct {
		SSHKey string `json:"ssh_public_key"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	gate := h.memberGate(id)
	if !gate.TryLock() {
		return 0, nil, httpapi.NewError(409, "资源操作正在执行，请稍后修改公钥")
	}
	defer gate.Unlock()
	err := h.Bastion.ChangeMemberKeys(id, req.SSHKey, func(tx *sql.Tx) error {
		// Include administrator-assigned containers on workers without a provisioning slot.
		if _, err := tx.Exec(`INSERT INTO member_key_sync(member_id,node_id,state)
 SELECT ?,id,'pending' FROM cluster_nodes WHERE kind='worker'
 ON CONFLICT(member_id,node_id) DO UPDATE SET state='pending',error=''`, id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE member_work SET pending=1 WHERE member_id=?", id); err != nil {
			return err
		}
		return platform.Audit(tx, id, "member.keys.change", id)
	})
	if err != nil {
		return 0, nil, err
	}
	h.wakeProvision()
	return 202, map[string]bool{"ok": true}, nil
}

// The existing provisioning queue owns retries and restart recovery; no extra
// timer is needed. Failed syncs are visible and can be retried by saving again.
func (h *Control) syncMemberKeys(ctx context.Context, id, username, keys string) error {
	rows, err := platform.Rows(h.DB.SQL, "SELECT node_id FROM member_key_sync WHERE member_id=? AND state<>'ready'", id)
	if err != nil {
		return err
	}
	var problems []error
	for _, row := range rows {
		nodeID := row["node_id"].(string)
		node, e := h.node(nodeID)
		if e == nil {
			callCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
			var out struct {
				OK bool `json:"ok"`
			}
			e = h.call(callCtx, node, "PATCH", "/api/containers/members/"+id,
				map[string]string{"username": username, "ssh_public_key": keys},
				platform.User{ID: id, Username: "member:" + username, Role: "admin"}, &out)
			cancel()
			if e == nil && !out.OK {
				e = httpapi.NewError(502, "节点未确认公钥已同步")
			}
		}
		state, message := "ready", ""
		if e != nil {
			state, message = "failed", e.Error()
		}
		_, persistErr := h.DB.SQL.Exec("UPDATE member_key_sync SET state=?,error=? WHERE member_id=? AND node_id=?", state, message, id, nodeID)
		problems = append(problems, e, persistErr)
	}
	return errors.Join(problems...)
}
