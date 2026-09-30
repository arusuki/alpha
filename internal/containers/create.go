package containers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"project-alpha/internal/platform"
)

var hostSSHPortPattern = regexp.MustCompile(`Port ([0-9]+)/`)

type CreateRequest struct {
	MemberID   string `json:"-"`
	SSHKey     string `json:"-"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Image      string `json:"image"`
	Network    string `json:"network"`
	Port       int    `json:"port"`
	GPUs       string `json:"gpus"`
	LocalProxy bool   `json:"local_proxy"`
	Password   string `json:"password"`
}

func password(value string) (string, error) {
	if value == "" {
		return platform.RandomHex(12), nil
	}
	if len(value) < 8 || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n:") {
		return "", fmt.Errorf("root 密码需为 8–256 字节，不能含换行、冒号或 NUL")
	}
	return value, nil
}
func validOwner(owner string) bool {
	return len(owner) > 0 && len(owner) <= 128 && !strings.ContainsAny(owner, "\x00\r\n")
}
func (h *Handler) create(ctx context.Context, cfg Config, req CreateRequest, actor string) (map[string]any, error) {
	if !validName.MatchString(req.Name) || req.Name == "data" {
		return nil, fmt.Errorf("容器名需为 1–63 位字母、数字、下划线、点或连字符，以字母或数字开头，且不能为 data")
	}
	if req.Owner == "" {
		req.Owner = req.Name
	}
	if !validOwner(req.Owner) {
		return nil, fmt.Errorf("所属用户需为 1–128 字节且不能含换行")
	}
	if req.Network == "" {
		req.Network = "bridge"
	}
	if req.Network != "host" && req.Network != "bridge" {
		return nil, fmt.Errorf("网络模式仅支持 host / bridge")
	}
	if req.LocalProxy && req.Network != "bridge" {
		return nil, fmt.Errorf("localhost 代理选项仅用于 bridge 模式")
	}
	if req.GPUs == "" {
		req.GPUs = "all"
	}
	g, e := strconv.Atoi(req.GPUs)
	if req.GPUs != "all" && (e != nil || g < 1 || g > 7) {
		return nil, fmt.Errorf("GPU 数量必须是 all 或 1–7")
	}
	if req.Image == "" {
		req.Image = cfg.Image
	}
	imageCfg := cfg
	imageCfg.Image = req.Image
	if req.Image == "" {
		return nil, fmt.Errorf("请先配置默认镜像或填写本次镜像")
	}
	if err := imageCfg.validate(); err != nil {
		return nil, err
	}
	pass, err := password(req.Password)
	if err != nil {
		return nil, err
	}
	daemon, err := h.daemon(ctx, cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	records, err := h.records()
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, r := range records {
		if r.Endpoint == cfg.Endpoint && r.Daemon == daemon {
			if r.Owner == req.Owner {
				return nil, fmt.Errorf("该使用者在此 node 已有容器 %s", r.Name)
			}
			if r.Name == req.Name {
				return nil, fmt.Errorf("名称 %s 已有管理记录，请处理原记录", req.Name)
			}
			used[r.Spec.Port] = true
		}
	}
	// Include stopped containers: their configured ports remain reserved here.
	ids, err := h.run(ctx, cfg.Endpoint, []string{"ps", "-aq", "--no-trunc"}, "")
	if err != nil {
		return nil, err
	}
	for _, id := range strings.Fields(ids) {
		c, e := h.inspect(ctx, cfg.Endpoint, id)
		if e != nil {
			return nil, e
		}
		if strings.TrimPrefix(c.Name, "/") == req.Name {
			return nil, fmt.Errorf("容器 %s 已存在，请通过命令行导入", req.Name)
		}
		for _, bindings := range c.HostConfig.PortBindings {
			for _, b := range bindings {
				if p, e := strconv.Atoi(b.HostPort); e == nil {
					used[p] = true
				}
			}
		}
		// Reserve SSH ports encoded in host-network container commands.
		if c.HostConfig.NetworkMode == "host" {
			for _, match := range hostSSHPortPattern.FindAllStringSubmatch(strings.Join(c.Config.Cmd, " "), -1) {
				p, _ := strconv.Atoi(match[1])
				used[p] = true
			}
		}
	}
	port := req.Port
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("SSH 端口必须为 1–65535，或 0 自动分配")
	}
	if port == 0 {
		port = cfg.StartPort
		for p := range used {
			if p >= port {
				port = p + 1
			}
		}
	}
	if port > 65535 {
		return nil, fmt.Errorf("没有可分配的 SSH 端口")
	}
	if used[port] {
		return nil, fmt.Errorf("SSH 端口 %d 已被容器配置占用", port)
	}
	// A short-lived bind tests non-Docker listeners as well. Docker still performs
	// the authoritative bind at start; any race is surfaced as a start failure.
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, fmt.Errorf("SSH 端口 %d 不可用: %w", port, err)
	}
	listener.Close()
	if _, err = h.run(ctx, cfg.Endpoint, []string{"image", "inspect", req.Image}, ""); err != nil {
		return nil, fmt.Errorf("镜像不可用，请先在本机准备镜像: %w", err)
	}
	gate := "/run/project-alpha-" + platform.RandomHex(16) + ".ready"
	userDir := filepath.Join(cfg.BaseDir, req.Name)
	reuse := false
	if req.MemberID != "" {
		plan, e := h.memberPlan(req.MemberID)
		if e != nil {
			return nil, e
		}
		if plan.Endpoint != "" {
			gate = plan.Gate
		} else {
			req.Port = port
			if e = h.saveMemberPlan(req.MemberID, memberPlan{Request: req, Endpoint: cfg.Endpoint, Daemon: daemon, Gate: gate, BaseDir: cfg.BaseDir}); e != nil {
				return nil, e
			}
		}
		raw, e := os.ReadFile(filepath.Join(userDir, ".project-alpha-member"))
		reuse = e == nil && string(raw) == req.MemberID
	}
	if _, err = os.Lstat(userDir); err == nil && !reuse {
		return nil, fmt.Errorf("数据目录 %s 已存在且不属于本次分配，不覆盖", userDir)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err = os.MkdirAll(cfg.BaseDir, 0755); err != nil {
		return nil, err
	}
	if err = directory(cfg.BaseDir); err != nil {
		return nil, err
	}
	if !reuse {
		if err = os.Mkdir(userDir, 0755); err != nil {
			return nil, err
		}
		if req.MemberID != "" {
			if err = os.WriteFile(filepath.Join(userDir, ".project-alpha-member"), []byte(req.MemberID), 0600); err != nil {
				return nil, err
			}
		}
	}
	if err = directory(userDir); err != nil {
		return nil, err
	}
	for _, path := range []string{filepath.Join(userDir, "workspace"), filepath.Join(userDir, "home"), filepath.Join(cfg.BaseDir, "data")} {
		if err = os.MkdirAll(path, 0755); err != nil {
			return nil, err
		}
		if err = directory(path); err != nil {
			return nil, err
		}
	}
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 1; done; /usr/sbin/sshd && touch %s.started && exec /bin/bash", gate, gate)
	args := []string{"create", "--pull", "never", "--name", req.Name, "--hostname", "docker-" + req.Name, "--network", req.Network, "--ipc", "host", "--restart", "unless-stopped", "--tty", "--interactive", "--ulimit", "memlock=-1:-1", "--gpus", "driver=nvidia,count=" + req.GPUs, "--user", "root", "--label", "project-alpha.owner=" + req.Owner, "--entrypoint", "/bin/bash"}
	if req.MemberID != "" {
		args = append(args, "--label", "project-alpha.member="+req.MemberID)
	}
	for _, dest := range []string{"workspace", "home", "data"} {
		source := filepath.Join(userDir, dest)
		if dest == "data" {
			source = filepath.Join(cfg.BaseDir, "data")
		}
		args = append(args, "--mount", "type=bind,src="+source+",dst=/"+dest)
	}
	if req.Network == "bridge" {
		args = append(args, "--publish", strconv.Itoa(port)+":22")
	}
	if req.LocalProxy {
		args = append(args, "--env", "HTTP_PROXY=http://localhost:7890", "--env", "HTTPS_PROXY=http://localhost:7890")
	}
	args = append(args, req.Image, "-c", script)
	out, err := h.run(ctx, cfg.Endpoint, args, "")
	if err != nil {
		return nil, fmt.Errorf("创建失败；数据目录已保留，请核对容器和目录后重试: %w", err)
	}
	id := strings.TrimSpace(out)
	if !fullID.MatchString(id) {
		return nil, fmt.Errorf("Docker create 未返回完整 ID；请刷新并检查 %s 的实际状态", req.Name)
	}
	c, err := h.inspect(ctx, cfg.Endpoint, id)
	if err != nil {
		return nil, h.rollbackCreated(cfg.Endpoint, id, err)
	}
	r := Record{ID: id, Endpoint: cfg.Endpoint, Daemon: daemon, Name: req.Name, Owner: req.Owner, Spec: Spec{req.Image, req.Network, port, req.GPUs, req.LocalProxy, cfg.BaseDir}, Fingerprint: fingerprint(c), Origin: "create", Gate: gate}
	if err = h.save(r, actor); err != nil {
		return nil, h.rollbackCreated(cfg.Endpoint, id, err)
	}
	if _, err = h.run(ctx, cfg.Endpoint, []string{"start", id}, ""); err != nil {
		return nil, fmt.Errorf("容器 %s 已登记但启动失败；可重试初始化: %w", req.Name, err)
	}
	if req.MemberID != "" {
		if err = h.installMemberKey(ctx, r, req.MemberID, req.SSHKey); err != nil {
			return nil, err
		}
	}
	if err = h.initialize(ctx, r, pass, actor); err != nil {
		return nil, fmt.Errorf("容器 %s 已登记，SSH 尚未启用；请重试初始化: %w", req.Name, err)
	}
	return map[string]any{"id": id, "name": req.Name, "port": port, "password": pass}, nil
}
func (h *Handler) initialize(ctx context.Context, r Record, pass, actor string) error {
	port := 22
	if r.Spec.Network == "host" {
		port = r.Spec.Port
	}
	// Persist the SSH port in the new container's config so ordinary sshd -T
	// reports the same effective port during subsequent adoption checks.
	setup := fmt.Sprintf("command -v chpasswd && mkdir -p /run/sshd && sed -i '/^[[:space:]]*Port[[:space:]]/d' /etc/ssh/sshd_config && sed -i '1iPort %d\\nPermitRootLogin yes\\nPasswordAuthentication yes' /etc/ssh/sshd_config && /usr/sbin/sshd -t", port)
	_, err := h.run(ctx, r.Endpoint, []string{"exec", r.ID, "/bin/sh", "-c", setup}, "")
	if err != nil {
		return fmt.Errorf("镜像的 chpasswd、sed 或 SSH 配置检查失败: %w", err)
	}
	out, err := h.run(ctx, r.Endpoint, []string{"exec", r.ID, "/usr/sbin/sshd", "-T"}, "")
	if err != nil {
		return fmt.Errorf("读取新容器 SSH 配置失败: %w", err)
	}
	ports := sshPorts(out)
	if len(ports) != 1 || ports[0] != port {
		return fmt.Errorf("新容器 SSH 端口应为 %d，实际为 %v；请检查镜像中 SSH Include 配置", port, ports)
	}
	_, err = h.run(ctx, r.Endpoint, []string{"exec", "-i", r.ID, "/bin/sh", "-c", `set -e; chpasswd; touch "$1"; i=0; while [ ! -f "$1.started" ] && [ "$i" -lt 10 ]; do sleep 1; i=$((i+1)); done; test -f "$1.started"`, "sh", r.Gate}, "root:"+pass+"\n")
	if err != nil {
		return fmt.Errorf("密码设置或 SSH 启动失败，请检查容器运行状态后重试初始化: %w", err)
	}

	return h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE managed_containers SET initialized=1 WHERE id=?", r.ID); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "container.initialize", r.Name+" ("+r.ID+")")
	})
}

// Only the newly created, still unregistered container is eligible for rollback.
// Docker rm has neither --force nor --volumes; all bind data remains intact.
func (h *Handler) rollbackCreated(endpoint, id string, cause error) error {
	_, err := h.run(context.Background(), endpoint, []string{"rm", id}, "")
	if err != nil {
		return fmt.Errorf("登记失败: %v；新容器 %s 清理也失败: %v。请手动核对，数据目录已保留", cause, id, err)
	}
	return fmt.Errorf("登记失败: %v；本次新建容器已撤销，数据目录已保留，请核对目录后再创建", cause)
}
