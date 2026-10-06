package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type permissionStatus struct {
	Username        string `json:"username"`
	UID             int    `json:"uid"`
	Endpoint        string `json:"endpoint"`
	BaseDir         string `json:"base_dir"`
	GroupMember     bool   `json:"group_member"`
	GroupActive     bool   `json:"group_active"`
	GroupError      string `json:"group_error"`
	RestartRequired bool   `json:"restart_required"`
	DirectoryWrite  bool   `json:"directory_writable"`
	DirectoryError  string `json:"directory_error"`
	DockerAvailable bool   `json:"docker_available"`
	DockerError     string `json:"docker_error"`
	SudoError       string `json:"sudo_error"`
}

func groupMembership(u *user.User, group *user.Group, active []int, primary int) (bool, bool, error) {
	ids, err := u.GroupIds()
	if err != nil {
		return false, false, err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return false, false, err
	}
	return u.Gid == group.Gid || slices.Contains(ids, group.Gid), primary == gid || slices.Contains(active, gid), nil
}

func (h *Handler) permissionStatus(ctx context.Context, cfg Config) (permissionStatus, error) {
	s := permissionStatus{UID: os.Geteuid(), Endpoint: cfg.Endpoint, BaseDir: cfg.BaseDir}
	u, err := user.LookupId(strconv.Itoa(s.UID))
	if err != nil {
		return s, fmt.Errorf("无法查询 worker 运行账号")
	}
	s.Username = u.Username
	group, err := user.LookupGroup("docker")
	if err != nil {
		s.GroupError = "系统中没有可查询的 docker 组"
	} else {
		active, e := os.Getgroups()
		if e == nil {
			s.GroupMember, s.GroupActive, e = groupMembership(u, group, active, os.Getegid())
		}
		if e != nil {
			s.GroupError = "无法查询账号或当前进程的组列表"
		}
	}
	s.RestartRequired = s.UID != 0 && s.GroupMember && !s.GroupActive
	if err := probeDirectory(cfg.BaseDir); err != nil {
		s.DirectoryError = err.Error()
	} else {
		s.DirectoryWrite = true
	}
	if _, err := h.daemon(ctx, cfg.Endpoint); err != nil {
		s.DockerError = err.Error()
	} else {
		s.DockerAvailable = true
	}
	if _, err := os.Stat("/usr/bin/sudo"); err != nil {
		s.SudoError = "节点未安装 /usr/bin/sudo，请由节点管理员安装"
	} else if raw, err := os.ReadFile("/proc/self/status"); err == nil && s.UID != 0 {
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "NoNewPrivs:" && fields[1] == "1" {
				s.SudoError = "worker 的 NoNewPrivileges 已启用，无法使用 sudo；请调整服务配置并重启 worker"
			}
		}
	}
	return s, nil
}

// Open every component without following symlinks. The returned descriptor pins
// the directory during both probes and privileged ACL changes.
func openPermissionDirectory(path string, create bool) (*os.File, error) {
	if !cleanPath(path) || path == "/" {
		return nil, fmt.Errorf("数据根目录必须是非根目录的规范绝对路径")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, e := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e == unix.ENOENT && create && i == len(parts)-1 {
			if e = unix.Mkdirat(fd, part, 0750); e == nil || e == unix.EEXIST {
				next, e = unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		unix.Close(fd)
		if e != nil {
			return nil, fmt.Errorf("目录 %s 不可访问（需真实目录，父目录必须已存在）: %w", path, e)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func probeDirectory(path string) error {
	dir, err := openPermissionDirectory(path, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	name := ".project-alpha-permission-" + platform.RandomHex(16)
	if err := unix.Mkdirat(int(dir.Fd()), name, 0700); err != nil {
		return fmt.Errorf("无法在 %s 创建目录: %w", path, err)
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("无法移除权限检查目录 %s/%s: %w", path, name, err)
	}
	return nil
}

func (h *Handler) permissions(w http.ResponseWriter, r *http.Request, actor platform.User, cfg Config) (int, any, error) {
	if actor.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		s, err := h.permissionStatus(r.Context(), cfg)
		return 200, s, err
	}
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "仅支持 GET / POST")
	}
	// Decode secret-bearing fields without reflecting invalid values in errors.
	value, err := httpapi.RequestBody(w, r)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		for _, raw := range value {
			clear(raw)
		}
	}()
	fields := map[string]string{}
	for key, raw := range value {
		if key != "action" && key != "base_dir" && key != "endpoint" && key != "sudo_password" {
			return 0, nil, httpapi.NewError(400, "权限修复请求字段无效")
		}
		var v string
		if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
			return 0, nil, httpapi.NewError(400, "权限修复请求字段必须是字符串")
		}
		fields[key] = v
	}
	password := []byte(fields["sudo_password"])
	delete(fields, "sudo_password")
	defer clear(password)
	if len(password) > 4096 || strings.ContainsAny(string(password), "\x00\r\n") {
		return 0, nil, httpapi.NewError(400, "sudo 密码格式无效")
	}
	if fields["base_dir"] != cfg.BaseDir || fields["endpoint"] != cfg.Endpoint {
		return 0, nil, httpapi.NewError(409, "容器配置已变化，请重新检查权限后再修复")
	}
	action := fields["action"]
	if action != "docker_group" && action != "directory" {
		return 0, nil, httpapi.NewError(400, "请选择 Docker 组或数据目录权限")
	}
	detail := action + " / uid=" + strconv.Itoa(os.Geteuid()) + " / " + cfg.BaseDir
	if err := platform.Audit(h.db.SQL, actor.Username, "container.permissions.start", detail); err != nil {
		return 0, nil, err
	}
	err = runSudoPermissions(r.Context(), password, permissionRepair{Action: action, UID: os.Geteuid(), BaseDir: cfg.BaseDir}, h.permissionCommand)
	result := "container.permissions.completed"
	if err != nil {
		result = "container.permissions.failed"
	}
	if auditErr := platform.Audit(h.db.SQL, actor.Username, result, detail); auditErr != nil {
		return 0, nil, httpapi.NewError(500, "权限操作已尝试，但审计写入失败；请重新检查节点权限")
	}
	if err != nil {
		return 0, nil, err
	}
	s, err := h.permissionStatus(r.Context(), cfg)
	return 200, s, err
}
