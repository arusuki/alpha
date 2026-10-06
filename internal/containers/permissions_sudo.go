package containers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/sudoutil"
)

const permissionPrompt = "[project-alpha-permissions-password]"

type permissionRepair struct {
	Action  string `json:"action"`
	UID     int    `json:"uid"`
	BaseDir string `json:"base_dir"`
}

type permissionEvent struct {
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
}

var permissionErrors = map[string]string{
	"invalid":         "权限修复范围无效或与 sudo 调用账号不一致",
	"account":         "无法查询 worker 的系统账号",
	"group":           "创建 docker 组失败，请检查节点的本地账号管理工具",
	"usermod":         "加入 docker 组失败，请检查 usermod 和系统账号配置",
	"setfacl_missing": "节点未安装 /usr/bin/setfacl，请先安装 acl 软件包",
	"directory":       "无法准备数据目录；请确认父目录已存在、路径不含符号链接且文件系统可写",
	"acl":             "数据目录 ACL 设置失败，请检查文件系统是否支持 POSIX ACL",
}

// Only a prompt response carries the secret. It is never placed in argv, env,
// helper requests, diagnostics, or audit records. NOPASSWD also works.
func runSudoPermissions(ctx context.Context, password []byte, request permissionRepair, command func(context.Context, string, ...string) *exec.Cmd) error {
	defer clear(password)
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位权限辅助程序")
	}
	if command == nil {
		command = exec.CommandContext
	}
	childCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	cmd := command(childCtx, "/usr/bin/sudo", "-S", "-k", "-p", permissionPrompt, "--", executable, "container-permissions-helper")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	input, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("无法创建 sudo 输入管道")
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("无法创建权限结果管道")
	}
	defer output.Close()
	prompt := sudoutil.NewInput(input, password, permissionPrompt)
	defer prompt.Clear()
	cmd.Stderr = prompt
	cmd.Cancel = func() error { _ = input.Close(); return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return httpapi.NewError(503, "无法启动 sudo，请检查节点安装及服务配置")
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 1024), 4096)
	read := func() (permissionEvent, bool) {
		var event permissionEvent
		ok := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &event) == nil
		return event, ok
	}
	ready, ok := read()
	if !ok || ready.Type != "ready" || ready.Code != "" {
		return httpapi.NewError(403, "sudo 认证失败或无执行权限；请使用 worker 运行账号的 sudo 密码，并检查 sudoers、NoNewPrivileges 和 requiretty 配置；密码未保存")
	}
	prompt.Clear()
	if err := json.NewEncoder(input).Encode(request); err != nil {
		return fmt.Errorf("无法发送权限修复请求")
	}
	result, ok := read()
	if !ok || result.Type != "result" {
		return fmt.Errorf("权限修复中断，请重新检查节点权限")
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		return fmt.Errorf("权限辅助程序异常退出，请重新检查节点权限")
	}
	if result.Code != "" {
		message, ok := permissionErrors[result.Code]
		if !ok {
			return fmt.Errorf("权限辅助程序返回未知状态")
		}
		return httpapi.NewError(409, message)
	}
	return nil
}

// The helper receives no password and opens no application database. Loss of
// the parent pipe cancels commands even when the worker cannot signal root.
func ServePermissionsHelper(parent context.Context, input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("权限辅助程序需要 sudo")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	encoder := json.NewEncoder(output)
	if err := encoder.Encode(permissionEvent{Type: "ready"}); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(input, 16*1024))
	raw, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("未收到权限修复请求")
	}
	var req permissionRepair
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || !validPermissionRepair(req, os.Getenv("SUDO_UID")) {
		return encoder.Encode(permissionEvent{Type: "result", Code: "invalid"})
	}
	go func() { _, _ = io.Copy(io.Discard, reader); cancel() }()
	code := applyPermissionRepair(ctx, req, exec.CommandContext)
	return encoder.Encode(permissionEvent{Type: "result", Code: code})
}

func validPermissionRepair(req permissionRepair, sudoUID string) bool {
	uid, err := strconv.Atoi(sudoUID)
	return err == nil && uid >= 0 && req.UID == uid &&
		(req.Action == "docker_group" || req.Action == "directory") &&
		cleanPath(req.BaseDir) && req.BaseDir != "/" && !strings.ContainsAny(req.BaseDir, ",:")
}

func applyPermissionRepair(ctx context.Context, req permissionRepair, command func(context.Context, string, ...string) *exec.Cmd) string {
	u, err := user.LookupId(strconv.Itoa(req.UID))
	if err != nil || u.Username == "" || strings.HasPrefix(u.Username, "-") || strings.ContainsAny(u.Username, "\x00\r\n") {
		return "account"
	}
	if req.Action == "docker_group" {
		if req.UID == 0 {
			return ""
		}
		if err := command(ctx, "/usr/sbin/groupadd", "--force", "docker").Run(); err != nil {
			return "group"
		}
		if err := command(ctx, "/usr/sbin/usermod", "--append", "--groups", "docker", "--", u.Username).Run(); err != nil {
			return "usermod"
		}
		return ""
	}
	if _, err := os.Stat("/usr/bin/setfacl"); err != nil {
		return "setfacl_missing"
	}
	dir, err := openPermissionDirectory(req.BaseDir, true)
	if err != nil {
		return "directory"
	}
	defer dir.Close()
	// ExtraFiles pins the inode for setfacl, including if the original path is
	// replaced. Grant only this account access to the configured directory.
	entry := "u:" + strconv.Itoa(req.UID) + ":rwx"
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(dir.Fd()), &stat); err != nil {
		return "directory"
	}
	if stat.Uid == uint32(req.UID) {
		entry = "u::rwx"
	}
	cmd := command(ctx, "/usr/bin/setfacl", "--modify", entry, "--", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{dir}
	if err := cmd.Run(); err != nil {
		return "acl"
	}
	return ""
}
