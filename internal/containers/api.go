package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Handler struct {
	db                *platform.Database
	run               command
	mu                sync.Mutex
	permissionCommand func(context.Context, string, ...string) *exec.Cmd
	// Owner integrates the platform's shared ownership overlay in the same transaction.
	Owner func(*sql.Tx, string, string) error
	// UnassignOwner clears shared and managed ownership in the same transaction.
	UnassignOwner func(*sql.Tx, string) error
}

func NewHandler(db *platform.Database) *Handler {
	return &Handler{db: db, run: runDocker}
}
func IsRoute(path string) bool {
	return path == "/api/containers" || strings.HasPrefix(path, "/api/containers/")
}
func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	fail := func(err error) (int, any, error) { return 0, nil, httpapi.NewError(409, err.Error()) }
	if r.Method != "GET" && user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if !h.mu.TryLock() {
		return 0, nil, httpapi.NewError(409, "容器管理正在执行其他操作，请稍后刷新或重试")
	}
	defer h.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	cfg, err := h.config()
	if err != nil {
		return fail(err)
	}
	path := r.URL.Path
	if path == "/api/containers/permissions" {
		return h.permissions(w, r.WithContext(ctx), user, cfg)
	}
	if strings.HasPrefix(path, "/api/containers/members/") {
		return h.memberOperation(w, r.WithContext(ctx), user, cfg, strings.TrimPrefix(path, "/api/containers/members/"))
	}
	if path == "/api/containers/settings" {
		if user.Role != "admin" {
			return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
		}
		if r.Method == "GET" {
			return 200, cfg, nil
		}
		if r.Method == "PUT" {
			var next Config
			if err = httpapi.DecodeBody(w, r, &next); err != nil {
				return 0, nil, err
			}
			if err = next.validate(); err != nil {
				return fail(err)
			}
			records, e := h.records()
			if e != nil {
				return fail(e)
			}
			if len(records) > 0 && next.Endpoint != cfg.Endpoint {
				return fail(fmt.Errorf("已有容器管理记录，不能切换 Docker endpoint；请先解除所有接管"))
			}
			err = h.db.Transaction(func(tx *sql.Tx) error {
				raw, _ := json.Marshal(next)
				if _, e := tx.Exec("UPDATE container_settings SET value=? WHERE id=1", string(raw)); e != nil {
					return e
				}
				return platform.Audit(tx, user.Username, "container.settings", "修改容器创建配置")
			})
			if err != nil {
				return fail(err)
			}
			return 200, next, nil
		}
	}
	if path == "/api/containers" && r.Method == "GET" {
		v, e := h.list(ctx, cfg)
		if e != nil {
			return fail(e)
		}
		return 200, v, nil
	}
	if path == "/api/containers" && r.Method == "POST" {
		var req CreateRequest
		if err = httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		value, e := h.create(ctx, cfg, req, user.Username)
		if e != nil {
			return fail(e)
		}
		return 201, value, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/containers/"), "/")
	if len(parts) == 2 && fullID.MatchString(parts[0]) && r.Method == "POST" {
		action := parts[1]
		if action != "start" && action != "stop" && action != "restart" && action != "delete" && action != "release" && action != "initialize" {
			return 0, nil, httpapi.NewError(404, "接口不存在")
		}
		var req struct {
			Confirm  string `json:"confirm"`
			Password string `json:"password"`
		}
		if err = httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		records, e := h.records()
		if e != nil {
			return fail(e)
		}
		for _, record := range records {
			if record.ID != parts[0] {
				continue
			}
			if (action == "delete" || action == "release") && req.Confirm != record.Name {
				return fail(fmt.Errorf("请填写完整容器名 %s 确认操作", record.Name))
			}
			if action == "release" { // Releasing a missing container never calls Docker.
				err = h.removeRecord(record, user.Username, "release")
				if err != nil {
					return fail(err)
				}
				return 200, map[string]bool{"ok": true}, nil
			}
			c, e := h.verify(ctx, record)
			if e != nil {
				return fail(e)
			}
			if action == "initialize" {
				if record.Initialized || record.Gate == "" {
					return fail(fmt.Errorf("此容器不需要初始化"))
				}
				pass, e := password(req.Password)
				if e != nil {
					return fail(e)
				}
				if !c.State.Running {
					if _, e = h.run(ctx, record.Endpoint, []string{"start", record.ID}, ""); e != nil {
						return fail(e)
					}
				}
				if e = h.initialize(ctx, record, pass, user.Username); e != nil {
					return fail(e)
				}
				return 200, map[string]any{"password": pass, "port": record.Spec.Port, "name": record.Name}, nil
			}
			if !record.Initialized && action != "delete" && action != "stop" {
				return fail(fmt.Errorf("容器尚未完成密码初始化，请先执行初始化"))
			}
			if c.State.Paused || c.State.Restarting || c.State.Dead {
				return fail(fmt.Errorf("容器当前为 %s，请先在 Docker 中恢复正常状态", c.State.Status))
			}
			args := []string{action, record.ID}
			if action == "delete" {
				if c.State.Running {
					return fail(fmt.Errorf("容器仍在运行，请先停止，再删除；不会强制删除"))
				}
				args = []string{"rm", record.ID}
			}
			if action == "stop" || action == "restart" {
				args = []string{action, "--time", "10", record.ID}
			}
			if _, err = h.run(ctx, record.Endpoint, args, ""); err != nil {
				return fail(err)
			}
			if action == "delete" {
				err = h.removeRecord(record, user.Username, action)
			} else {
				err = platform.Audit(h.db.SQL, user.Username, "container."+action, record.Name+" ("+record.ID+")")
			}
			if err != nil {
				return fail(fmt.Errorf("Docker 操作已完成，但管理记录/审计写入失败，请刷新核对: %w", err))
			}
			return 200, map[string]bool{"ok": true}, nil
		}
		return 0, nil, httpapi.NewError(404, "容器未接管，不能执行管理操作")
	}
	return 0, nil, httpapi.NewError(404, "接口不存在")
}
func (h *Handler) removeRecord(r Record, actor, action string) error {
	return h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM managed_containers WHERE id=?", r.ID); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "container."+action, r.Name+" ("+r.ID+")")
	})
}
func (h *Handler) verify(ctx context.Context, r Record) (inspection, error) {
	daemon, err := h.daemon(ctx, r.Endpoint)
	if err != nil {
		return inspection{}, err
	}
	if daemon != r.Daemon {
		return inspection{}, fmt.Errorf("Docker daemon 身份改变，拒绝操作容器 %s", r.Name)
	}
	c, err := h.inspect(ctx, r.Endpoint, r.ID)
	if err != nil {
		return c, fmt.Errorf("无法核实原容器 %s（%s），不会使用同名容器替代: %w", r.Name, r.ID, err)
	}
	if fingerprint(c) != r.Fingerprint {
		return c, fmt.Errorf("容器 %s 配置已在平台外修改；请解除接管后用命令行重新导入", r.Name)
	}
	for _, m := range c.Mounts {
		if m.Type == "bind" {
			if err = directory(m.Source); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}
func (h *Handler) list(ctx context.Context, cfg Config) (any, error) {
	records, err := h.records()
	if err != nil {
		return nil, err
	}
	daemon, err := h.daemon(ctx, cfg.Endpoint)
	if err != nil {
		for i := range records {
			records[i].Error = err.Error()
		}
		return map[string]any{"managed": records, "error": err.Error()}, nil
	}
	for i := range records {
		record := &records[i]
		if record.Endpoint != cfg.Endpoint || record.Daemon != daemon {
			record.Error = "Docker endpoint 或 daemon 身份不匹配"
			continue
		}
		c, e := h.inspect(ctx, record.Endpoint, record.ID)
		if e != nil {
			record.Error = e.Error()
			continue
		}
		record.State = c.State.Status
		if fingerprint(c) != record.Fingerprint {
			record.Error = "容器配置已在平台外修改，请解除接管后用命令行重新导入"
		}
	}
	return map[string]any{"managed": records, "ssh_host": cfg.SSHHost, "proxy_jump": cfg.ProxyJump}, nil
}
