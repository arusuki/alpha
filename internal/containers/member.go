package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

type memberPlan struct {
	Request  CreateRequest `json:"request"`
	Endpoint string        `json:"endpoint"`
	Daemon   string        `json:"daemon"`
	Gate     string        `json:"gate"`
	BaseDir  string        `json:"base_dir"`
}

func (h *Handler) memberPlan(id string) (memberPlan, error) {
	var p memberPlan
	var raw string
	err := h.db.SQL.QueryRow("SELECT plan FROM member_container_slots WHERE member_id=?", id).Scan(&raw)
	if err != nil {
		return p, err
	}
	if raw != "" {
		err = json.Unmarshal([]byte(raw), &p)
	}
	return p, err
}
func (h *Handler) saveMemberPlan(id string, p memberPlan) error {
	p.Request.Password = "" // Secrets are supplied again by control on retry.
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = h.db.SQL.Exec("UPDATE member_container_slots SET plan=? WHERE member_id=?", string(raw), id)
	return err
}
func (h *Handler) installMemberKey(ctx context.Context, r Record, id, key string) error {
	value, err := sshkeys.Rewrite(nil, id, key)
	if err != nil {
		return err
	}
	// The file is inside this member's dedicated container. Refuse links and use
	// stdin for key material. The live file is replaced atomically before sshd opens.
	script := `set -eu; test ! -L /root/.ssh; mkdir -p /root/.ssh; chmod 700 /root/.ssh; test ! -L /root/.ssh/authorized_keys; umask 077; target=/root/.ssh/authorized_keys; tmp=/root/.ssh/.alpha-key; test ! -L "$tmp"; touch "$target"; awk -v m="$1" '$0 != "# " m && $NF != m' "$target" > "$tmp"; cat >> "$tmp"; chmod 600 "$tmp"; mv "$tmp" "$target"`
	_, err = h.run(ctx, r.Endpoint, []string{"exec", "-i", r.ID, "/bin/sh", "-c", script, "sh", sshkeys.Marker(id)}, string(value))
	return err
}
func (h *Handler) memberOperation(w http.ResponseWriter, r *http.Request, u platform.User, cfg Config, id string) (int, any, error) {
	fail := func(e error) (int, any, error) { return 0, nil, httpapi.NewError(409, e.Error()) }
	if u.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if !sshkeys.ID.MatchString(id) {
		return 0, nil, httpapi.NewError(400, "使用者 ID 无效")
	}
	var req struct {
		Password      string `json:"password"`
		Mode          string `json:"mode"`
		ContainerID   string `json:"container_id"`
		ExpectedOwner string `json:"expected_owner"`
		Username      string `json:"username"`
		SSHKey        string `json:"ssh_public_key"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if r.Method != "PUT" && r.Method != "DELETE" && r.Method != "PATCH" {
		return 0, nil, httpapi.NewError(405, "不支持该方法")
	}
	if !validOwner(req.Username) {
		return fail(fmt.Errorf("使用者标识无效"))
	}
	if r.Method == "PATCH" || r.Method == "DELETE" {
		var owner string
		var deleted bool
		err := h.db.SQL.QueryRow("SELECT username,deleted FROM member_container_slots WHERE member_id=?", id).Scan(&owner, &deleted)
		if err != nil && err != sql.ErrNoRows {
			return fail(err)
		}
		if err == nil && owner != req.Username {
			return fail(fmt.Errorf("使用者身份不匹配"))
		}
		if deleted {
			if r.Method == "PATCH" {
				return fail(fmt.Errorf("该使用者已删除，不能修改公钥"))
			}
			return 200, map[string]bool{"ok": true}, nil
		}
		keys := ""
		if r.Method == "PATCH" {
			keys, err = sshkeys.NormalizeList(req.SSHKey)
			if err != nil {
				return fail(err)
			}
		}
		if err = h.syncOwnerKeys(r.Context(), req.Username, id, keys); err != nil {
			return fail(err)
		}
		if r.Method == "PATCH" {
			return 200, map[string]bool{"ok": true}, nil
		}
	}
	if r.Method == "DELETE" {
		err := h.db.Transaction(func(tx *sql.Tx) error {
			var username string
			var deleted bool
			err := tx.QueryRow("SELECT username,deleted FROM member_container_slots WHERE member_id=?", id).Scan(&username, &deleted)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && username != req.Username {
				return fmt.Errorf("使用者身份不匹配")
			}
			if deleted {
				return nil
			}
			if _, err = tx.Exec("UPDATE member_container_slots SET deleted=1 WHERE member_id=?", id); err != nil {
				return err
			}
			if h.UnassignOwner != nil {
				if err = h.UnassignOwner(tx, req.Username); err != nil {
					return err
				}
			}
			if h.UnassignOwner == nil {
				rows, err := platform.Rows(tx, "SELECT id FROM managed_containers WHERE owner=?", req.Username)
				if err != nil {
					return err
				}
				for _, row := range rows {
					if h.Owner != nil {
						if err = h.Owner(tx, row["id"].(string), ""); err != nil {
							return err
						}
					}
				}
				if _, err = tx.Exec("UPDATE managed_containers SET owner='' WHERE owner=?", req.Username); err != nil {
					return err
				}
			}
			if _, err = tx.Exec("INSERT INTO member_container_slots(member_id,username,plan,deleted) VALUES(?,?,'',1) ON CONFLICT(member_id) DO UPDATE SET deleted=1", id, req.Username); err != nil {
				return err
			}
			return platform.Audit(tx, u.Username, "member.containers.unassign", req.Username+" / "+id)
		})
		if err != nil {
			return fail(err)
		}
		return 200, map[string]bool{"ok": true}, nil
	}
	if req.Mode != "create" && req.Mode != "adopt" || req.Mode == "create" && req.ContainerID != "" || req.Mode == "adopt" && !fullID.MatchString(req.ContainerID) {
		return fail(fmt.Errorf("请选择新建或领养，领养须提供完整容器 ID"))
	}
	if err := members.ValidatePassword(req.Password); err != nil {
		return 0, nil, err
	}
	var err error
	req.SSHKey, err = sshkeys.NormalizeList(req.SSHKey)
	if err != nil {
		return fail(err)
	}
	err = h.db.Transaction(func(tx *sql.Tx) error {
		// Validate the target and reserve it together; rejected unmanaged targets
		// must not leave a slot that blocks this member's later choice.
		if req.Mode == "adopt" {
			var exists bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM managed_containers WHERE id=?)", req.ContainerID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("容器未接管，请先通过 CLI 导入")
			}
		}
		_, err := tx.Exec("INSERT INTO member_container_slots(member_id,username,plan,deleted,mode,container_id) VALUES(?,?,'',0,?,?) ON CONFLICT(member_id) DO NOTHING", id, req.Username, req.Mode, req.ContainerID)
		return err
	})
	if err != nil {
		if platform.IsConstraint(err) {
			return fail(fmt.Errorf("容器已领养或该使用者在此 node 已有分配"))
		}
		return fail(err)
	}
	var owner, mode, target string
	var deleted bool
	if err = h.db.SQL.QueryRow("SELECT username,deleted,mode,container_id FROM member_container_slots WHERE member_id=?", id).Scan(&owner, &deleted, &mode, &target); err != nil {
		return fail(err)
	}
	if owner != req.Username {
		return fail(fmt.Errorf("使用者身份不匹配"))
	}
	if deleted {
		return fail(fmt.Errorf("该使用者已删除，不能重新分配资源"))
	}
	if mode != req.Mode || mode == "adopt" && target != req.ContainerID {
		return fail(fmt.Errorf("已有分配计划，不能更改领养或新建选择"))
	}
	if mode == "adopt" {
		return h.adoptMember(r.Context(), cfg, id, req.Username, req.SSHKey, target, req.ExpectedOwner, u.Username)
	}
	plan, err := h.memberPlan(id)
	if err != nil {
		return fail(err)
	}
	ctx := r.Context()
	if plan.Endpoint != "" {
		daemon, e := h.daemon(ctx, plan.Endpoint)
		if e != nil {
			return fail(e)
		}
		if daemon != plan.Daemon {
			return fail(fmt.Errorf("Docker daemon 身份改变，不能重试或回收"))
		}
	}
	records, err := h.records()
	if err != nil {
		return fail(err)
	}
	var record Record
	for _, v := range records {
		if v.Name == "alpha-"+id {
			record = v
		} else if v.Owner == owner {
			return fail(fmt.Errorf("该使用者在此 node 已有容器 %s", v.Name))
		}
	}
	if record.ID != "" && (plan.Endpoint == "" || record.Owner != owner || record.Endpoint != plan.Endpoint || record.Daemon != plan.Daemon) {
		return fail(fmt.Errorf("同名管理容器不属于本次分配，拒绝操作"))
	}
	// Recover the narrow window between Docker create and saving the management row.
	if record.ID == "" && plan.Endpoint != "" {
		ids, e := h.run(ctx, plan.Endpoint, []string{"ps", "-aq", "--no-trunc"}, "")
		if e != nil {
			return fail(e)
		}
		for _, cid := range strings.Fields(ids) {
			c, e := h.inspect(ctx, plan.Endpoint, cid)
			if e != nil {
				return fail(e)
			}
			if strings.TrimPrefix(c.Name, "/") != plan.Request.Name {
				continue
			}
			if err := checkMemberContainer(c, plan, id); err != nil {
				return fail(err)
			}
			record = Record{ID: c.ID, Endpoint: plan.Endpoint, Daemon: plan.Daemon, Name: plan.Request.Name, Owner: owner, Spec: Spec{plan.Request.Image, plan.Request.Network, plan.Request.Port, plan.Request.GPUs, plan.Request.LocalProxy, plan.BaseDir}, Fingerprint: fingerprint(c), Origin: "create", Gate: plan.Gate}
			if e = h.save(record, u.Username); e != nil {
				return fail(e)
			}
		}
	}
	if record.ID == "" {
		next := CreateRequest{Name: "alpha-" + id, Owner: owner}
		if plan.Endpoint != "" {
			next = plan.Request
			cfg.Endpoint = plan.Endpoint
			cfg.BaseDir = plan.BaseDir
		}
		next.MemberID = id
		next.SSHKey = req.SSHKey
		next.Password = req.Password
		value, e := h.create(ctx, cfg, next, u.Username)
		if e != nil {
			return fail(e)
		}
		delete(value, "password")
		value["ssh_host"] = cfg.SSHHost
		return 200, value, nil
	}
	c, err := h.verify(ctx, record)
	if err != nil {
		return fail(err)
	}
	if err = checkMemberContainer(c, plan, id); err != nil {
		return fail(err)
	}
	if !record.Initialized {
		if !c.State.Running {
			if _, err = h.run(ctx, record.Endpoint, []string{"start", record.ID}, ""); err != nil {
				return fail(err)
			}
		}
		if err = h.installMemberKey(ctx, record, id, req.SSHKey); err != nil {
			return fail(err)
		}
		if err = h.initialize(ctx, record, req.Password, u.Username); err != nil {
			return fail(err)
		}
	}
	return 200, map[string]any{"id": record.ID, "name": record.Name, "port": record.Spec.Port, "ssh_host": cfg.SSHHost}, nil
}

// Recovery must not legitimize an unrelated or externally reconfigured container.
func checkMemberContainer(c inspection, p memberPlan, id string) error {
	bad := fmt.Errorf("容器身份或配置与成员创建计划不符，拒绝接管或初始化")
	if c.Config.Labels["project-alpha.member"] != id || c.Config.Labels["project-alpha.owner"] != p.Request.Owner || c.Config.Image != p.Request.Image || c.HostConfig.NetworkMode != p.Request.Network || !strings.Contains(strings.Join(c.Config.Cmd, " "), p.Gate) || len(c.Mounts) != 3 {
		return bad
	}
	expected := map[string]string{"/workspace": filepath.Join(p.BaseDir, p.Request.Name, "workspace"), "/home": filepath.Join(p.BaseDir, p.Request.Name, "home"), "/data": filepath.Join(p.BaseDir, "data")}
	for _, m := range c.Mounts {
		if m.Type != "bind" || !m.RW || expected[m.Destination] != m.Source {
			return bad
		}
		delete(expected, m.Destination)
	}
	if len(expected) != 0 {
		return bad
	}
	return nil
}

// Clear member-marked keys before releasing ownership. If Docker is unavailable
// or a container is stopped, keep the ownership and report a retryable failure.
func (h *Handler) syncOwnerKeys(ctx context.Context, username, id, keys string) error {
	records, err := h.records()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Owner != username {
			continue
		}
		c, err := h.verify(ctx, record)
		if err != nil {
			return err
		}
		if !c.State.Running {
			return fmt.Errorf("容器 %s 已停止，请启动后重试公钥同步或删除", record.Name)
		}
		if err = h.installMemberKey(ctx, record, id, keys); err != nil {
			return err
		}
	}
	return nil
}
