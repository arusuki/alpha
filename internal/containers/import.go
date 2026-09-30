package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type ImportOptions struct {
	Endpoint, BaseDir string
	DryRun            bool
	Containers        []string
}

// Import scans Docker and registers matching containers without changing them.
// Each record, ownership overlay, settings and audit entry commit together.
func (h *Handler) Import(ctx context.Context, opts ImportOptions, out io.Writer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	original, err := h.config()
	if err != nil {
		return err
	}
	cfg := original
	if opts.Endpoint != "" {
		cfg.Endpoint = opts.Endpoint
	}
	if opts.BaseDir != "" {
		cfg.BaseDir = opts.BaseDir
	}
	if err = cfg.validate(); err != nil {
		return err
	}
	records, err := h.records()
	if err != nil {
		return err
	}
	if len(records) > 0 && cfg.Endpoint != original.Endpoint {
		return fmt.Errorf("已有容器管理记录，不能切换 Docker endpoint；请先解除所有接管")
	}
	daemon, err := h.daemon(ctx, cfg.Endpoint)
	if err != nil {
		return err
	}
	refs := opts.Containers
	if len(refs) == 0 {
		ids, err := h.run(ctx, cfg.Endpoint, []string{"ps", "-a", "--no-trunc", "--quiet"}, "")
		if err != nil {
			return err
		}
		refs = strings.Fields(ids)
		for _, id := range refs {
			if !fullID.MatchString(id) {
				return fmt.Errorf("Docker 容器列表未返回完整 ID")
			}
		}
	}
	managed := map[string]Record{}
	for _, r := range records {
		managed[r.ID] = r
	}
	seen := map[string]bool{}
	imported, skipped, failed, ready := 0, 0, 0, 0
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := h.inspect(ctx, cfg.Endpoint, ref)
		if err != nil {
			fmt.Fprintf(out, "失败 %s：%v\n", ref, err)
			failed++
			continue
		}
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		if r, ok := managed[c.ID]; ok && r.Endpoint == cfg.Endpoint && r.Daemon == daemon {
			fmt.Fprintf(out, "跳过 %s (%s)：已登记\n", r.Name, r.ID)
			skipped++
			continue
		}
		owner := strings.TrimPrefix(c.Name, "/")
		report, record := h.check(ctx, cfg, c.ID, owner)
		// Bind the check to the daemon and configuration originally discovered.
		if report.OK && (record.Daemon != daemon || record.Fingerprint != fingerprint(c)) {
			report.add("容器身份", false, "扫描期间 Docker daemon 或容器配置发生变化，请重试")
		}
		if !report.OK {
			fmt.Fprintf(out, "失败 %s (%s)\n", owner, c.ID)
			for _, check := range report.Checks {
				if !check.OK {
					fmt.Fprintf(out, "  %s：%s\n", check.Name, check.Reason)
				}
			}
			failed++
			continue
		}
		if opts.DryRun {
			fmt.Fprintf(out, "可导入 %s (%s)：所属用户 %s，SSH %d\n", record.Name, record.ID, record.Owner, record.Spec.Port)
			ready++
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err = h.db.Transaction(func(tx *sql.Tx) error {
			var raw string
			if err := tx.QueryRow("SELECT value FROM container_settings WHERE id=1").Scan(&raw); err != nil {
				return err
			}
			var current Config
			if err := json.Unmarshal([]byte(raw), &current); err != nil {
				return err
			}
			if current != original {
				return fmt.Errorf("容器配置已被其他进程修改，请重新运行导入")
			}
			value, _ := json.Marshal(cfg)
			if _, err := tx.Exec("UPDATE container_settings SET value=? WHERE id=1", string(value)); err != nil {
				return err
			}
			return h.saveTx(tx, record, "cli")
		})
		if err != nil {
			return fmt.Errorf("登记 %s 失败（此前已导入 %d 个）：%w", record.Name, imported, err)
		}
		original = cfg
		imported++
		fmt.Fprintf(out, "已导入 %s (%s)：所属用户 %s，SSH %d\n", record.Name, record.ID, record.Owner, record.Spec.Port)
	}
	fmt.Fprintf(out, "导入 %d，可导入 %d，已登记跳过 %d，失败 %d\n", imported, ready, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d 个容器未通过导入检查；已成功导入的记录保留", failed)
	}
	return nil
}
