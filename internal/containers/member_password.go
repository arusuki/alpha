package containers

import (
	"database/sql"
	"fmt"
	"net/http"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

// PasswordResetResult also reports partial writes, which cannot be rolled back
// across Docker containers. Retrying with the same password is safe.
type PasswordResetResult struct {
	OK      bool     `json:"ok"`
	Updated int      `json:"updated"`
	Errors  []string `json:"errors"`
}

func (h *Handler) resetMemberPassword(w http.ResponseWriter, r *http.Request, user platform.User, id string) (int, any, error) {
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "不支持该方法")
	}
	if !sshkeys.ID.MatchString(id) {
		return 0, nil, httpapi.NewError(400, "使用者 ID 无效")
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if !validOwner(req.Username) {
		return 0, nil, httpapi.NewError(400, "使用者标识无效")
	}
	if err := members.ValidatePassword(req.Password); err != nil {
		return 0, nil, err
	}
	var owner string
	var deleted bool
	err := h.db.SQL.QueryRow("SELECT username,deleted FROM member_container_slots WHERE member_id=?", id).Scan(&owner, &deleted)
	if err != nil && err != sql.ErrNoRows {
		return 0, nil, err
	}
	if err == nil && (owner != req.Username || deleted) {
		return 0, nil, httpapi.NewError(409, "使用者身份不匹配或已删除")
	}
	// Include manually assigned and adopted containers, even without a member slot.
	records, err := h.records()
	if err != nil {
		return 0, nil, err
	}
	result := PasswordResetResult{OK: true, Errors: []string{}}
	for _, record := range records {
		if record.Owner != req.Username {
			continue
		}
		change := func() error {
			c, err := h.verify(r.Context(), record)
			if err != nil {
				return err
			}
			if !c.State.Running {
				return fmt.Errorf("容器已停止，请启动后重试")
			}
			// Keep the secret out of argv, environment, Docker config and diagnostics.
			if _, err = h.run(r.Context(), record.Endpoint, []string{"exec", "-i", "--user", "0", record.ID, "chpasswd"}, "root:"+req.Password+"\n"); err != nil {
				return fmt.Errorf("密码设置失败，请检查容器状态后重试（密码未回显）")
			}
			result.Updated++
			return h.db.Transaction(func(tx *sql.Tx) error {
				return platform.Audit(tx, user.Username, "container.password.reset", record.ID)
			})
		}
		if err := change(); err != nil {
			result.OK = false
			result.Errors = append(result.Errors, record.Name+" / "+record.ID+"："+err.Error())
		}
	}
	return 200, result, nil
}
