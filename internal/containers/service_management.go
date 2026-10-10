package containers

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const servicesPath = "/api/containers/services"
const rootlessService = "rootless-docker"

type DRAMConfig struct {
	Backend    string  `json:"backend"`
	IntervalUS int     `json:"interval_us"`
	PeakGBPS   float64 `json:"peak_gbps"`
}

type ServiceConfig struct {
	DRAM          *DRAMConfig `json:"dram,omitempty"`
	RestartPolicy string      `json:"restart_policy"`
	StopTimeout   int         `json:"stop_timeout"`
	ControlSocket string      `json:"control_socket"`
	SocketPath    string      `json:"socket_path"`
	Users         []string    `json:"users"`
}

func serviceConfigDefault(name string) ServiceConfig {
	c := ServiceConfig{RestartPolicy: "unless-stopped", StopTimeout: 10, Users: []string{}}
	if name == "dram-bw" {
		c.DRAM = &DRAMConfig{Backend: "amd-rome", IntervalUS: 100000}
	}
	if name == rootlessService {
		c.StopTimeout = 120
		c.ControlSocket = "/run/rootless-docker/control.sock"
		c.SocketPath = "/var/run/docker.sock"
	}
	return c
}
func serviceName(name string) bool {
	return name == "tetragon" || name == "dram-bw" || name == rootlessService
}
func (c ServiceConfig) validate(name string) error {
	if name == "dram-bw" {
		if c.DRAM == nil || !slices.Contains([]string{"auto", "amd-rome", "mock"}, c.DRAM.Backend) {
			return errors.New("DRAM 采集后端无效，请选择 auto、amd-rome 或 mock")
		}
		if c.DRAM.IntervalUS < 100 || c.DRAM.IntervalUS > 10000000 {
			return errors.New("DRAM 采样间隔必须为 100–10000000 微秒")
		}
		if math.IsNaN(c.DRAM.PeakGBPS) || math.IsInf(c.DRAM.PeakGBPS, 0) || c.DRAM.PeakGBPS < 0 || c.DRAM.PeakGBPS > 1000000 {
			return errors.New("DRAM 参考峰值必须为 0–1000000 GB/s，0 表示不设置")
		}
	} else if c.DRAM != nil {
		return errors.New("此服务不支持 DRAM 采集参数")
	}
	if !slices.Contains([]string{"no", "always", "unless-stopped", "on-failure"}, c.RestartPolicy) {
		return errors.New("重启策略无效")
	}
	if c.StopTimeout < 1 || c.StopTimeout > 180 {
		return errors.New("停止等待时间必须为 1–180 秒")
	}
	if c.Users == nil || len(c.Users) > 1000 {
		return errors.New("使用者列表无效")
	}
	seen := map[string]bool{}
	for _, user := range c.Users {
		if !validOwner(user) || seen[user] {
			return errors.New("使用者标识无效或重复")
		}
		seen[user] = true
	}
	if name != rootlessService {
		if c.ControlSocket != "" || c.SocketPath != "" || len(c.Users) != 0 {
			return errors.New("此服务不向使用者容器提供 socket 挂载")
		}
	} else {
		for _, p := range []string{c.ControlSocket, c.SocketPath} {
			if !cleanPath(p) || p == "/" {
				return errors.New("socket 必须是规范的绝对文件路径")
			}
		}
		for _, p := range []string{"/proc", "/sys", "/dev"} {
			if c.SocketPath == p || strings.HasPrefix(c.SocketPath, p+"/") {
				return errors.New("socket 目标不能位于 /proc、/sys 或 /dev")
			}
		}
	}
	return nil
}
func (h *Handler) serviceConfig(name string) (ServiceConfig, error) {
	c := serviceConfigDefault(name)
	var raw string
	err := h.db.SQL.QueryRow("SELECT config FROM node_service_settings WHERE name=?", name).Scan(&raw)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	c = ServiceConfig{}
	if err = decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("服务配置损坏，原数据已保留：%w", err)
	}
	return c, c.validate(name)
}
func (h *Handler) saveServiceConfig(name string, c ServiceConfig, actor string) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO node_service_settings(name,config) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET config=excluded.config", name, string(raw)); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "service.settings", name)
	})
}

// Resolve labels to exactly one immutable ID, then recheck it with inspect.
// A missing service is a deployment error; the web API never creates a substitute.
func (h *Handler) resolveService(ctx context.Context, endpoint, name string) (inspection, error) {
	raw, err := h.run(ctx, endpoint, []string{"container", "ls", "--all", "--no-trunc", "--filter", "label=project-alpha.service=" + name, "--format", "{{.ID}}"}, "")
	if err != nil {
		return inspection{}, err
	}
	ids := strings.Fields(raw)
	if len(ids) == 0 {
		return inspection{}, fmt.Errorf("%s 未部署，请先安装节点 Compose 服务", name)
	}
	if len(ids) != 1 || !fullID.MatchString(ids[0]) {
		return inspection{}, fmt.Errorf("%s 服务容器不唯一或 ID 无效，拒绝操作", name)
	}
	c, err := h.inspect(ctx, endpoint, ids[0])
	if err != nil {
		return c, err
	}
	if c.ID != ids[0] || c.Config.Labels["project-alpha.service"] != name {
		return c, errors.New("服务容器身份改变，拒绝操作")
	}
	return c, nil
}

func serviceStateError(c inspection, name string) error {
	if c.State.Paused || c.State.Dead || c.State.Status == "removing" || c.State.Restarting && name != "dram-bw" {
		return fmt.Errorf("服务当前为 %s，请先在 Docker 中恢复正常状态", c.State.Status)
	}
	return nil
}

func callRootless(ctx context.Context, socket string, args []string) (string, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	data, _ := json.Marshal(map[string]any{"args": args})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://rootless/_rootless/control", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return "", fmt.Errorf("Rootless 管理 socket 不可用：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Rootless 管理请求失败（%d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return result.Output, errors.New(result.Error)
	}
	return result.Output, nil
}
func (h *Handler) waitRootless(ctx context.Context, c ServiceConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var last error
	for {
		attempt, stop := context.WithTimeout(ctx, 3*time.Second)
		_, last = h.rootlessControl(attempt, c.ControlSocket, []string{"status"})
		stop()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("服务容器已启动，但 Rootless 尚未就绪；可重试应用挂载：%w", last)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// The image entrypoint reads this file on every start. Copying works for both
// running and stopped containers and preserves all other deployment settings.
func (h *Handler) applyDRAMConfig(ctx context.Context, endpoint string, service inspection, c ServiceConfig) error {
	if service.Config.Labels["project-alpha.dram-settings"] != "1" || !slices.Equal(service.Config.Entrypoint, []string{"/usr/local/bin/dram-bwd", "--settings", "/run/dram-bw/service-settings"}) {
		return errors.New("DRAM 镜像不支持网页配置，请重新构建并部署 dram-bw 服务")
	}
	payload := fmt.Sprintf("%s %d %s\n", c.DRAM.Backend, c.DRAM.IntervalUS, strconv.FormatFloat(c.DRAM.PeakGBPS, 'g', -1, 64))
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "service-settings", Mode: 0644, Size: int64(len(payload))}); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, payload); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	_, err := h.serviceRun(ctx, endpoint, []string{"cp", "-", service.ID + ":/run/dram-bw/"}, archive.String())
	return err
}

func (h *Handler) verifyDRAMStarted(ctx context.Context, endpoint, id string) error {
	// Catch immediate startup failures before reporting success.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	c, err := h.resolveService(ctx, endpoint, "dram-bw")
	if err != nil {
		return err
	}
	if c.ID != id || !c.State.Running || c.State.Restarting || c.State.Paused || c.State.Dead {
		return errors.New("DRAM 启动后未保持运行，请检查采集后端、硬件权限及服务日志")
	}
	return nil
}

func (h *Handler) serviceAction(ctx context.Context, endpoint, name, action, id string, timeout int) error {
	args := []string{action}
	if action == "stop" || action == "restart" {
		args = append(args, "--time", strconv.Itoa(timeout))
	}
	if _, err := h.serviceRun(ctx, endpoint, append(args, id), ""); err != nil {
		return err
	}
	if name == "dram-bw" && action != "stop" {
		return h.verifyDRAMStarted(ctx, endpoint, id)
	}
	return nil
}

func (h *Handler) applyServiceConfig(ctx context.Context, endpoint, name string, service inspection, c ServiceConfig) error {
	if name == "dram-bw" && service.State.Restarting {
		// Stop the restart loop before replacing the file read at startup.
		if err := h.serviceAction(ctx, endpoint, name, "stop", service.ID, c.StopTimeout); err != nil {
			return fmt.Errorf("停止 DRAM 重启循环失败：%w", err)
		}
	}
	if _, err := h.serviceRun(ctx, endpoint, []string{"update", "--restart", c.RestartPolicy, service.ID}, ""); err != nil {
		return fmt.Errorf("Docker 策略应用失败：%w", err)
	}
	if name == "dram-bw" {
		if err := h.applyDRAMConfig(ctx, endpoint, service, c); err != nil {
			return fmt.Errorf("采集参数下发失败：%w", err)
		}
	}
	return nil
}

func (h *Handler) dispatchServices(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	fail := func(err error) (int, any, error) { return 0, nil, httpapi.NewError(409, err.Error()) }
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "节点服务管理需要管理员权限")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6 * time.Minute))
	cfg, err := h.config()
	if err != nil {
		return fail(err)
	}
	if r.URL.Path == servicesPath && r.Method == "GET" {
		v, e := h.servicesView(ctx)
		if e != nil {
			return fail(e)
		}
		return 200, v, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, servicesPath+"/"), "/")
	if len(parts) != 2 || !serviceName(parts[0]) {
		return 0, nil, httpapi.NewError(404, "服务接口不存在")
	}
	name, action := parts[0], parts[1]
	saving := action == "settings" && r.Method == "PUT"
	if !saving && (r.Method != "POST" || !slices.Contains([]string{"start", "stop", "restart", "apply"}, action)) {
		return 0, nil, httpapi.NewError(405, "不支持该服务操作")
	}
	if action == "apply" && name != rootlessService {
		return fail(errors.New("此服务不提供使用者挂载"))
	}
	current, err := h.serviceConfig(name)
	if err != nil {
		return fail(err)
	}
	if saving {
		var next ServiceConfig
		if err = httpapi.DecodeBody(w, r, &next); err != nil {
			return 0, nil, err
		}
		if err = next.validate(name); err != nil {
			return fail(err)
		}
		if name == rootlessService {
			mounts, e := h.serviceMounts()
			if e != nil {
				return fail(e)
			}
			if current.ControlSocket != next.ControlSocket && len(mounts) > 0 {
				return fail(errors.New("请先取消所有受管理挂载，再更换管理 socket"))
			}
			// Revoke access before committing the new selection. Failed removals stay
			// journaled and block owner reassignment until a retry succeeds.
			if err = h.revokeServiceMounts(ctx, current, next, false); err != nil {
				return fail(err)
			}
		}
		if err = h.saveServiceConfig(name, next, user.Username); err != nil {
			return fail(err)
		}
		current = next
		fail = func(err error) (int, any, error) {
			return 0, nil, httpapi.NewError(409, "配置已保存，应用未完成："+err.Error())
		}
	}
	c, err := h.resolveService(ctx, cfg.Endpoint, name)
	if err != nil {
		return fail(err)
	}
	if err = serviceStateError(c, name); err != nil {
		return fail(err)
	}
	if name == rootlessService && (action == "stop" || action == "restart") {
		if err = h.revokeServiceMounts(ctx, current, current, true); err != nil {
			return fail(fmt.Errorf("未能撤销挂载，尚未停止服务：%w", err))
		}
	}
	if action != "stop" {
		if err = h.applyServiceConfig(ctx, cfg.Endpoint, name, c, current); err != nil {
			return fail(err)
		}
	}
	runAction := action
	if saving {
		runAction = ""
		if name == "dram-bw" && (c.State.Running || c.State.Restarting) {
			runAction = "restart"
		}
	}
	if runAction != "" && runAction != "apply" {
		if err = h.serviceAction(ctx, cfg.Endpoint, name, runAction, c.ID, current.StopTimeout); err != nil {
			return fail(err)
		}
	}
	if name == rootlessService && action != "stop" && (!saving || c.State.Running) {
		if !saving {
			if err = h.waitRootless(ctx, current); err != nil {
				return fail(err)
			}
		}
		if err = h.reconcileServiceMounts(ctx, current); err != nil {
			return fail(fmt.Errorf("服务已就绪，部分挂载未完成：%w", err))
		}
	}
	if !saving {
		if err = platform.Audit(h.db.SQL, user.Username, "service."+action, name); err != nil {
			return fail(fmt.Errorf("服务操作已完成，审计写入失败：%w", err))
		}
	}
	return 200, map[string]bool{"ok": true}, nil
}
