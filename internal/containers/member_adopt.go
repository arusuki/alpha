package containers

import (
	"context"
	"database/sql"
	"fmt"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"strings"
)

// Adoption binds a full Docker identity and only adds the member key. It never
// recreates, initializes, starts, or changes the password of an existing container.
func (h *Handler) adoptMember(ctx context.Context, cfg Config, id, username, key, target, expectedOwner, actor string) (int, any, error) {
	fail := func(err error) (int, any, error) { return 0, nil, httpapi.NewError(409, err.Error()) }
	records, err := h.records()
	if err != nil {
		return fail(err)
	}
	var record Record
	for _, v := range records {
		if v.ID == target {
			record = v
		} else if v.Owner == username {
			return fail(fmt.Errorf("该使用者在此 node 已有容器 %s", v.Name))
		}
	}
	// Include live containers not yet imported or scanned, so choosing adoption
	// cannot bypass the per-node limit through an incomplete inventory.
	refs, err := h.run(ctx, cfg.Endpoint, []string{"ps", "-aq", "--no-trunc"}, "")
	if err != nil {
		return fail(err)
	}
	for _, ref := range strings.Fields(refs) {
		if ref == target {
			continue
		}
		c, e := h.inspect(ctx, cfg.Endpoint, ref)
		if e != nil {
			return fail(e)
		}
		owner := inspectionOwner(c)
		if h.ReadOwner != nil {
			owner, e = h.ReadOwner(c.ID, owner)
			if e != nil {
				return fail(e)
			}
		}
		if owner == username {
			return fail(fmt.Errorf("该使用者在此 node 已有容器 %s", strings.TrimPrefix(c.Name, "/")))
		}
	}
	if record.ID == "" {
		c, err := h.inspect(ctx, cfg.Endpoint, target)
		if err != nil {
			return fail(err)
		}
		owner := inspectionOwner(c)
		if h.ReadOwner != nil {
			owner, err = h.ReadOwner(c.ID, owner)
			if err != nil {
				return fail(err)
			}
		}
		if owner != expectedOwner {
			return fail(fmt.Errorf("容器归属已变化，请刷新后重试"))
		}
		report, checked := h.check(ctx, cfg, target, username)
		if !report.OK {
			reasons := []string{}
			for _, check := range report.Checks {
				if !check.OK {
					reasons = append(reasons, check.Name+": "+check.Reason)
				}
			}
			return fail(fmt.Errorf("容器不符合领养要求：%s", strings.Join(reasons, "；")))
		}
		if checked.Fingerprint != fingerprint(c) {
			return fail(fmt.Errorf("容器配置在领养检查期间发生变化，请重试"))
		}
		if err := h.save(checked, actor); err != nil {
			return fail(err)
		}
		record = checked
		expectedOwner = username
	}
	if record.Owner != expectedOwner {
		return fail(fmt.Errorf("容器归属已变化，请刷新后重试"))
	}
	if !record.Initialized {
		return fail(fmt.Errorf("容器尚未完成初始化，不能领养"))
	}
	c, err := h.verify(ctx, record)
	if err != nil {
		return fail(err)
	}
	if !c.State.Running || c.State.Paused || c.State.Restarting || c.State.Dead {
		return fail(fmt.Errorf("请先启动或修复已有容器后重试领养"))
	}
	// Claim ownership before writing SSH keys. The durable slot makes retries
	// idempotent and prevents another member from adopting during key installation.
	err = h.db.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE managed_containers SET owner=? WHERE id=? AND owner=?", username, target, expectedOwner)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("容器归属已变化，请刷新后重试")
		}
		if h.Owner != nil {
			if err := h.Owner(tx, target, username); err != nil {
				return err
			}
		}
		return platform.Audit(tx, actor, "member.container.adopt", id+" / "+target)
	})
	if err != nil {
		return fail(err)
	}
	if err := h.installMemberKey(ctx, record, id, key); err != nil {
		return fail(err)
	}
	return 200, map[string]any{"id": record.ID, "name": record.Name, "port": record.Spec.Port, "ssh_host": cfg.SSHHost}, nil
}
