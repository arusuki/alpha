package bastion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/sudoutil"
)

const sudoInstallPrompt = "[project-alpha-bastion-password]"

type installRequest struct {
	Action      string `json:"action"`
	ServiceUser string `json:"service_user"`
	Directory   string `json:"directory"`
}

type installEvent struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

type webInstallInfo struct {
	ServiceUser string `json:"service_user"`
	Available   bool   `json:"available"`
	Error       string `json:"error"`
}

func installAvailability() webInstallInfo {
	v := webInstallInfo{}
	u, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		v.Error = "无法确定运行 control 的系统用户"
		return v
	}
	v.ServiceUser = u.Username
	if os.Geteuid() == 0 || u.Username == JumpUser {
		v.Error = "请使用独立的普通服务用户运行 control"
		return v
	}
	nnp, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil {
		v.Error = "无法检查当前服务的提权限制"
		return v
	}
	if nnp != 0 {
		v.Error = "当前服务禁止 sudo 提权；请将总控 systemd 的 NoNewPrivileges 设为 false 并重启，或在终端完成初始化"
		return v
	}
	info, err := os.Stat("/usr/bin/sudo")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		v.Error = "服务主机未安装可执行的 /usr/bin/sudo"
		return v
	}
	v.Available = true
	return v
}

// This endpoint is synchronous: no job, password or request body is persisted.
// Role/CSRF/Origin checks run before it; the server fixes the service and data directory.
func (h *Handler) installFromWeb(w http.ResponseWriter, r *http.Request, actor string) (int, any, error) {
	body, err := httpapi.RequestBody(w, r)
	defer func() {
		for _, raw := range body {
			clear(raw)
		}
	}()
	if err != nil {
		return 0, nil, err
	}
	var action, confirm string
	if json.Unmarshal(body["action"], &action) != nil || !validAccountAction(action) {
		return 0, nil, httpapi.NewError(400, "请选择 init、adopt、release 或 delete 操作")
	}
	fields := 2
	if action == "delete" {
		fields++
		if json.Unmarshal(body["confirm"], &confirm) != nil || confirm != JumpUser {
			return 0, nil, httpapi.NewError(400, "请输入 alpha-jump 确认删除账号；工具和 data 将保留")
		}
	}
	if len(body) != fields || body["sudo_password"] == nil {
		return 0, nil, httpapi.NewError(400, "只接受 action、本次 sudo_password 及删除时的 confirm")
	}
	var value string
	if json.Unmarshal(body["sudo_password"], &value) != nil {
		return 0, nil, httpapi.NewError(400, "请输入服务用户的 sudo 密码")
	}
	password := []byte(value)
	value = ""
	clear(body["sudo_password"])
	defer clear(password)
	if len(password) == 0 || len(password) > 1024 || bytes.ContainsAny(password, "\r\n\x00") {
		return 0, nil, httpapi.NewError(400, "sudo 密码需为 1–1024 字节，不能包含换行或 NUL")
	}
	if !h.installMu.TryLock() {
		return 0, nil, httpapi.NewError(409, "已有跳板账号操作正在执行，请等待完成")
	}
	defer h.installMu.Unlock()
	info := h.installInfo()
	if !info.Available {
		return 0, nil, httpapi.NewError(409, info.Error)
	}
	if err = platform.Audit(h.DB.SQL, actor, "bastion.account."+action+".start", JumpUser); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	err = h.installRunner(ctx, password, installRequest{Action: action, ServiceUser: info.ServiceUser, Directory: h.DB.Directory})
	clear(password)
	if err == nil && (action == "init" || action == "adopt") {
		err = h.installation()
	}
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if auditErr := platform.Audit(h.DB.SQL, actor, "bastion.account."+action+"."+status, JumpUser); auditErr != nil {
		return 0, nil, httpapi.NewError(500, "安装结果的审计写入失败，请刷新安装状态核对")
	}
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]bool{"ok": true}, nil
}

// Only sudo sees the password. Raw sudo/PAM stderr is discarded by Input;
// structured installer errors are read from stdout after the ready handshake.
func runSudoInstall(ctx context.Context, password []byte, request installRequest, command func(context.Context, string, ...string) *exec.Cmd) error {
	defer clear(password)
	if err := ctx.Err(); err != nil {
		return httpapi.NewError(408, "安装已取消")
	}
	executable, err := os.Executable()
	if err != nil {
		return httpapi.NewError(503, "无法定位安装程序")
	}
	if command == nil {
		command = exec.CommandContext
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := command(child, "/usr/bin/sudo", "-S", "-k", "-p", sudoInstallPrompt, "--", executable, "bastion", "install-helper")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	input, err := cmd.StdinPipe()
	if err != nil {
		return httpapi.NewError(503, "无法创建 sudo 输入管道")
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return httpapi.NewError(503, "无法创建安装结果管道")
	}
	defer output.Close()
	prompt := sudoutil.NewInput(input, password, sudoInstallPrompt)
	defer prompt.Clear()
	cmd.Stderr = prompt
	cmd.Cancel = func() error { _ = input.Close(); return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err = cmd.Start(); err != nil {
		return httpapi.NewError(503, "无法启动 sudo，请检查安装及服务用户的 sudo 权限")
	}
	waited := false
	defer func() {
		_ = input.Close()
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	timer := time.AfterFunc(30*time.Second, cancel)
	defer timer.Stop()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 32768)
	read := func() (installEvent, error) {
		var event installEvent
		if !scanner.Scan() || strictJSON(scanner.Bytes(), &event) != nil {
			return event, fmt.Errorf("安装程序未返回有效结果")
		}
		return event, nil
	}
	ready, err := read()
	if err != nil || ready.Type != "ready" || ready.Error != "" {
		return httpapi.NewError(403, "sudo 认证失败、超时或无执行权限；请重新输入服务用户的 sudo 密码，密码未保存")
	}
	prompt.Clear()
	timer.Stop()
	if child.Err() != nil {
		return httpapi.NewError(408, "安装已取消或超时，请刷新状态后重试")
	}
	if err = json.NewEncoder(input).Encode(request); err != nil {
		return httpapi.NewError(503, "无法发送安装请求")
	}
	result, err := read()
	if err != nil {
		return httpapi.NewError(503, "安装连接中断，请刷新状态核对后重试")
	}
	if result.Type != "done" && result.Type != "error" || result.Type == "done" && result.Error != "" || result.Type == "error" && result.Error == "" {
		return httpapi.NewError(503, "安装程序返回无效状态，请刷新核对")
	}
	err = cmd.Wait()
	waited = true
	if child.Err() != nil {
		return httpapi.NewError(408, "安装已取消或超时，请刷新状态后重试")
	}
	if err != nil {
		return httpapi.NewError(503, "安装程序异常退出，请刷新状态核对后重试")
	}
	if result.Type == "error" {
		return httpapi.NewError(409, result.Error)
	}
	return nil
}

func ServeInstallHelper(parent context.Context, input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装辅助程序需要 sudo 权限")
	}
	return serveInstallHelper(parent, input, output, func(ctx context.Context, request installRequest) error {
		// sudo, not browser input, determines whose installation this is.
		uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
		if err != nil || uid <= 0 {
			return fmt.Errorf("请通过普通服务用户的 sudo 执行网页安装")
		}
		u, err := user.LookupId(strconv.Itoa(uid))
		if err != nil || u.Username != request.ServiceUser {
			return fmt.Errorf("安装服务用户与 sudo 调用者不一致")
		}
		if !filepath.IsAbs(request.Directory) {
			return fmt.Errorf("总控数据目录必须为绝对路径")
		}
		return install(ctx, request.Action, request.ServiceUser, request.Directory, false, io.Discard)
	})
}

func serveInstallHelper(parent context.Context, input io.Reader, output io.Writer, action func(context.Context, installRequest) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	encoder := json.NewEncoder(output)
	if err := encoder.Encode(installEvent{Type: "ready"}); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(input, 16384))
	raw, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("缺少安装请求")
	}
	var request installRequest
	if strictJSON(raw, &request) != nil || !validAccountAction(request.Action) || request.ServiceUser == "" || request.Directory == "" {
		return fmt.Errorf("安装请求无效")
	}
	// EOF also cancels privileged work when its ordinary parent cannot signal
	// the root process or has crashed. No detached installer outlives the pipe.
	go func() { _, _ = io.Copy(io.Discard, reader); cancel() }()
	err = action(ctx, request)
	result := installEvent{Type: "done"}
	if err != nil {
		result.Type = "error"
		result.Error = strings.ToValidUTF8(err.Error(), "�")
	}
	return encoder.Encode(result)
}
