package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type ServiceMount struct {
	ContainerID string `json:"container_id"`
	Endpoint    string `json:"endpoint"`
	Daemon      string `json:"-"`
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	SocketPath  string `json:"socket_path"`
	Error       string `json:"error"`
	State       string `json:"state"`
}

type rootlessBinding struct {
	Host       string `json:"host"`
	Container  string `json:"container"`
	Name       string `json:"name"`
	SocketPath string `json:"socket_path"`
	Error      string `json:"error"`
	State      string `json:"state"`
}

func (h *Handler) serviceMounts() ([]ServiceMount, error) {
	rows, err := h.db.SQL.Query("SELECT container_id,endpoint,daemon,owner,name,socket_path,error FROM node_service_mounts ORDER BY owner,name,socket_path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceMount{}
	for rows.Next() {
		var m ServiceMount
		if err = rows.Scan(&m.ContainerID, &m.Endpoint, &m.Daemon, &m.Owner, &m.Name, &m.SocketPath, &m.Error); err != nil {
			return nil, err
		}
		m.State = "unknown"
		out = append(out, m)
	}
	return out, rows.Err()
}
func (h *Handler) rootlessBindings(ctx context.Context, c ServiceConfig) ([]rootlessBinding, error) {
	raw, err := h.rootlessControl(ctx, c.ControlSocket, []string{"bindings"})
	if err != nil {
		return nil, err
	}
	var out []rootlessBinding
	if err = json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return nil, errors.New("Rootless 挂载响应无效，请更新节点管理服务")
	}
	seen := map[string]bool{}
	for _, b := range out {
		key := b.Host + "\x00" + b.Container + "\x00" + b.SocketPath
		if !strings.HasPrefix(b.Host, "unix://") || !cleanPath(strings.TrimPrefix(b.Host, "unix://")) || !fullID.MatchString(b.Container) || !cleanPath(b.SocketPath) || b.SocketPath == "/" || !slices.Contains([]string{"mounted", "waiting", "pending", "missing", "unknown"}, b.State) || seen[key] {
			return nil, errors.New("Rootless 挂载响应字段无效或重复，原操作记录已保留")
		}
		seen[key] = true
	}
	return out, nil
}
func sameBinding(b rootlessBinding, m ServiceMount) bool {
	return b.Host == m.Endpoint && b.Container == m.ContainerID && b.SocketPath == m.SocketPath
}
func (h *Handler) rememberMountError(m ServiceMount, e error) error {
	detail := ""
	if e != nil {
		detail = e.Error()
	}
	_, err := h.db.SQL.Exec("UPDATE node_service_mounts SET error=? WHERE endpoint=? AND container_id=? AND socket_path=?", detail, m.Endpoint, m.ContainerID, m.SocketPath)
	return errors.Join(e, err)
}
func (h *Handler) revokeServiceMounts(ctx context.Context, current, next ServiceConfig, all bool) error {
	mounts, err := h.serviceMounts()
	if err != nil {
		return err
	}
	var revoked []ServiceMount
	for _, m := range mounts {
		if all || !slices.Contains(next.Users, m.Owner) || next.SocketPath != m.SocketPath {
			revoked = append(revoked, m)
		}
	}
	if len(revoked) == 0 {
		return nil
	}
	bindings, err := h.rootlessBindings(ctx, current)
	if err != nil {
		return fmt.Errorf("无法核实已有挂载，记录已保留；请恢复管理服务后重试：%w", err)
	}
	var errs []error
	for _, m := range revoked {
		present := slices.ContainsFunc(bindings, func(b rootlessBinding) bool { return sameBinding(b, m) })
		if present {
			daemon, e := h.daemon(ctx, m.Endpoint)
			if e == nil && daemon != m.Daemon {
				e = errors.New("Docker daemon 身份改变，拒绝卸载")
			}
			if e == nil {
				_, e = h.rootlessControl(ctx, current.ControlSocket, []string{"remove", "--host", m.Endpoint, m.ContainerID, "--socket-path", m.SocketPath})
			}
			if e != nil {
				errs = append(errs, h.rememberMountError(m, e))
				continue
			}
		}
		if _, e := h.db.SQL.Exec("DELETE FROM node_service_mounts WHERE endpoint=? AND container_id=? AND socket_path=?", m.Endpoint, m.ContainerID, m.SocketPath); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}
func (h *Handler) reconcileServiceMounts(ctx context.Context, c ServiceConfig) error {
	if err := h.revokeServiceMounts(ctx, c, c, false); err != nil {
		return err
	}
	records, err := h.records()
	if err != nil {
		return err
	}
	return h.attachServiceMounts(ctx, c, records)
}

func (h *Handler) attachServiceMounts(ctx context.Context, c ServiceConfig, records []Record) error {
	var errs []error
	for _, r := range records {
		if !slices.Contains(c.Users, r.Owner) || !r.Initialized {
			continue
		}
		m := ServiceMount{ContainerID: r.ID, Endpoint: r.Endpoint, Daemon: r.Daemon, Owner: r.Owner, Name: r.Name, SocketPath: c.SocketPath}
		// The transaction serializes the journal against ownership updates, including
		// updates through the separate storage API. Triggers protect it thereafter.
		err := h.db.Transaction(func(tx *sql.Tx) error {
			var owner string
			if e := tx.QueryRow("SELECT owner FROM managed_containers WHERE id=? AND endpoint=? AND daemon=?", r.ID, r.Endpoint, r.Daemon).Scan(&owner); e != nil {
				return e
			}
			if owner != r.Owner {
				return errors.New("容器使用者已改变，请刷新后重试")
			}
			_, e := tx.Exec("INSERT INTO node_service_mounts(container_id,endpoint,daemon,owner,name,socket_path) VALUES(?,?,?,?,?,?) ON CONFLICT(endpoint,container_id,socket_path) DO NOTHING", r.ID, r.Endpoint, r.Daemon, r.Owner, r.Name, c.SocketPath)
			return e
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, err = h.verify(ctx, r)
		if err == nil {
			_, err = h.rootlessControl(ctx, c.ControlSocket, []string{"add", "--host", r.Endpoint, r.ID, "--socket-path", c.SocketPath})
		}
		if err = h.rememberMountError(m, err); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ReconcileServices restores saved selections on worker startup or owner changes.
func (h *Handler) ReconcileServices(ctx context.Context) error {
	if !h.mu.TryLock() {
		return errors.New("容器或服务管理正在执行其他操作，请稍后重试应用挂载")
	}
	defer h.mu.Unlock()
	return h.reconcileRunningService(ctx, "")
}

// A completed container operation must retain its result, especially generated
// credentials, when the separate socket attachment fails.
func (h *Handler) containerServiceResult(ctx context.Context, id string, result map[string]any) map[string]any {
	if err := h.reconcileRunningService(ctx, id); err != nil {
		result["warning"] = "服务挂载尚未完成，请在节点服务中重试应用：" + err.Error()
	}
	return result
}

func (h *Handler) reconcileRunningService(ctx context.Context, id string) error {
	c, err := h.serviceConfig(rootlessService)
	if err != nil {
		return err
	}
	mounts, err := h.serviceMounts()
	if err != nil {
		return err
	}
	if len(c.Users) == 0 && len(mounts) == 0 {
		return nil
	}
	var selected []Record
	if id != "" {
		records, err := h.records()
		if err != nil {
			return err
		}
		for _, r := range records {
			if r.ID == id && r.Initialized && slices.Contains(c.Users, r.Owner) {
				selected = append(selected, r)
			}
		}
		if len(selected) == 0 {
			return nil
		}
	}
	cfg, err := h.config()
	if err != nil {
		return err
	}
	service, err := h.resolveService(ctx, cfg.Endpoint, rootlessService)
	if err != nil {
		return err
	}
	if err := serviceStateError(service, rootlessService); err != nil {
		return err
	}
	if !service.State.Running {
		return nil
	}
	if id != "" {
		return h.attachServiceMounts(ctx, c, selected)
	}
	return h.reconcileServiceMounts(ctx, c)
}
func (h *Handler) servicesView(ctx context.Context) (any, error) {
	type view struct {
		ServiceStatus
		Config ServiceConfig `json:"config"`
	}
	services := []view{}
	var rootlessRunning bool
	for _, status := range h.ServiceStatuses(ctx) {
		c, err := h.serviceConfig(status.Name)
		if err != nil {
			return nil, err
		}
		services = append(services, view{status, c})
		if status.Name == rootlessService {
			rootlessRunning = status.State == "running"
		}
	}
	mounts, err := h.serviceMounts()
	if err != nil {
		return nil, err
	}
	records, err := h.records()
	if err != nil {
		return nil, err
	}
	candidates := []map[string]any{}
	for _, r := range records {
		if r.Owner != "" {
			candidates = append(candidates, map[string]any{"owner": r.Owner, "name": r.Name, "container_id": r.ID, "initialized": r.Initialized})
		}
	}
	external := []rootlessBinding{}
	detail := ""
	if rootlessRunning {
		c, err := h.serviceConfig(rootlessService)
		if err != nil {
			return nil, err
		}
		query, stop := context.WithTimeout(ctx, 10*time.Second)
		bindings, e := h.rootlessBindings(query, c)
		stop()
		if e != nil {
			detail = e.Error()
		} else {
			for i := range mounts {
				mounts[i].State = "pending"
				for _, b := range bindings {
					if sameBinding(b, mounts[i]) {
						mounts[i].State = b.State
						mounts[i].Error = b.Error
						break
					}
				}
			}
			for _, b := range bindings {
				if !slices.ContainsFunc(mounts, func(m ServiceMount) bool { return sameBinding(b, m) }) {
					external = append(external, b)
				}
			}
		}
	} else {
		detail = "Rootless 管理服务未运行，无法核实实时挂载；以下保留操作记录。"
	}
	return map[string]any{"services": services, "mounts": mounts, "external_mounts": external, "mount_error": detail, "candidates": candidates}, nil
}

func (h *Handler) checkServiceMounts(id string) error {
	var exists bool
	if err := h.db.SQL.QueryRow("SELECT EXISTS(SELECT 1 FROM node_service_mounts WHERE container_id=?)", id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("请先在节点服务中取消此容器的服务挂载，再删除或解除接管")
	}
	return nil
}
