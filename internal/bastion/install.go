package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/platform"
)

const sshConfigPath = "/etc/ssh/sshd_config"
const jumpHome = "/var/empty/alpha-jump"
const workerHome = "/var/empty/alpha-worker"
const workerKeysPath = installationDirectory + "/worker_authorized_keys"
const configBegin = "# BEGIN project-alpha share-node"
const configEnd = "# END project-alpha share-node"

func accountSSHConfig(name string) string {
	common := `    AuthenticationMethods publickey
    PubkeyAuthentication yes
    TrustedUserCAKeys none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowStreamLocalForwarding no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTTY no
    PermitUserRC no
    PermitTunnel no
`
	if name == JumpUser {
		return "Match User alpha-jump\n" + common + `    AuthorizedKeysFile none
    AuthorizedKeysCommand /usr/local/libexec/project-alpha-jump bastion authorized-keys %u %U
    AuthorizedKeysCommandUser alpha-jump
    DisableForwarding no
    AllowTcpForwarding local
    PermitOpen any
    GatewayPorts no
    PermitListen none
    MaxSessions 0
    ForceCommand /bin/false
`
	}
	return "Match User alpha-worker\n" + common + fmt.Sprintf(`    AuthorizedKeysFile %s
    AuthorizedKeysCommand none
    DisableForwarding yes
    AllowTcpForwarding no
    GatewayPorts no
    PermitOpen none
    PermitListen none
    MaxSessions 4
    ForceCommand %s bastion command
`, workerKeysPath, readerExecutable)
}
func jumpSSHConfig() string {
	return configBegin + "\n" + accountSSHConfig(JumpUser) + accountSSHConfig(WorkerUser) + "Match all\n" + configEnd + "\n"
}
func removeManagedSSH(raw []byte) ([]byte, error) {
	s := string(raw)
	begin, end := strings.Index(s, configBegin), strings.Index(s, configEnd)
	if begin < 0 && end < 0 {
		return raw, nil
	}
	if begin < 0 || end < begin || strings.Count(s, configBegin) != 1 || strings.Count(s, configEnd) != 1 || strings.TrimSpace(s[end+len(configEnd):]) != "" {
		return nil, fmt.Errorf("sshd_config 的分享节点配置标记无效或未处于末尾")
	}
	return []byte(strings.TrimSuffix(s[:begin], "\n")), nil
}
func configuredSSH(raw []byte) ([]byte, error) {
	original, err := removeManagedSSH(raw)
	if err != nil {
		return nil, err
	}
	return []byte(string(original) + "\n" + jumpSSHConfig()), nil
}
func checkEffectiveSSH(raw []byte, name string) error {
	effective := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			effective[parts[0]] = parts[1]
		}
	}
	for _, line := range strings.Split(accountSSHConfig(name), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || parts[0] == "Match" {
			continue
		}
		if effective[strings.ToLower(parts[0])] != strings.Join(parts[1:], " ") {
			return fmt.Errorf("sshd 已有配置与 %s 的 %s 冲突", name, parts[0])
		}
	}
	return nil
}
func installRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func rootDirectory(path string, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if parent != path {
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			if err = rootDirectory(parent, 0755); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(st.Uid) != 0 || info.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("安装路径上级属主或权限无效: %s", parent)
			}
		}
	}
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return fmt.Errorf("安装路径不能经过符号链接: %s", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(st.Uid) != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("安装路径属主或权限无效: %s", path)
	}
	return os.Chmod(path, mode)
}
func rootFile(path string) ([]byte, os.FileMode, error) {
	r, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	raw, err := readPrivateFile(r, filepath.Base(path), 0, -1, 0022, 256<<20)
	if err != nil {
		return nil, 0, err
	}
	info, err := r.Stat(filepath.Base(path))
	if err != nil {
		return nil, 0, err
	}
	return raw, info.Mode().Perm(), nil
}
func atomicRootFile(path string, raw []byte, mode os.FileMode) error {
	if _, _, err := rootFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".alpha-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func validateSSH(ctx context.Context, raw []byte, checkAccounts bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(sshConfigPath), ".alpha-sshd-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(raw)
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if _, err = installRun(ctx, "/usr/sbin/sshd", "-t", "-f", tmp.Name()); err != nil {
		return err
	}
	if checkAccounts {
		for _, name := range []string{JumpUser, WorkerUser} {
			out, err := installRun(ctx, "/usr/sbin/sshd", "-T", "-f", tmp.Name(), "-C", "user="+name+",host=localhost,addr=127.0.0.1")
			if err != nil {
				return err
			}
			if err = checkEffectiveSSH(out, name); err != nil {
				return err
			}
		}
	}
	return nil
}
func sshUnit(ctx context.Context, noReload bool) (string, error) {
	if noReload {
		return "", nil
	}
	for _, name := range []string{"ssh.service", "sshd.service"} {
		if _, err := installRun(ctx, "systemctl", "is-active", "--quiet", name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("没有活动的 ssh/sshd 服务；自行管理 sshd 时使用 --no-reload")
}
func reloadSSH(ctx context.Context, unit string) error {
	if unit == "" {
		return nil
	}
	_, err := installRun(ctx, "systemctl", "reload", unit)
	return err
}

// CLI is used only by sshd's root-owned command/reader executable.
func CLI(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(out, `用法：
  project-alpha bastion <子命令>

内部子命令（由 sshd 调用）：
  authorized-keys <账号名> <UID>  读取成员授权公钥
  command                       执行 alpha-worker 公钥管理协议

使用 project-alpha bastion <子命令> --help 查看详细用法。
安装和卸载请使用 project-alpha share-node --help。
`)
		return nil
	}
	if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
		switch args[0] {
		case "authorized-keys":
			fmt.Fprint(out, "用法：\n  project-alpha bastion authorized-keys <账号名> <UID>\n\n由 sshd 的 AuthorizedKeysCommand 以 alpha-jump 运行，将成员授权公钥写入标准输出。\n")
			return nil
		case "command":
			fmt.Fprint(out, "用法：\n  project-alpha bastion command\n\n由 sshd 以 alpha-worker 运行，从标准输入读取公钥管理 JSON 请求并输出 JSON 响应。\nSSH_ORIGINAL_COMMAND 必须为 alpha-worker cmd。\n")
			return nil
		}
	}
	if len(args) == 3 && args[0] == "authorized-keys" {
		return systemKeyStore().authorizedKeys(args[1], args[2], out)
	}
	if len(args) == 1 && args[0] == "command" {
		return serveCommand(systemKeyStore(), os.Getenv("SSH_ORIGINAL_COMMAND"), os.Stdin, out)
	}
	return fmt.Errorf("请在 share node 使用 project-alpha share-node 初始化；加 --uninstall 撤销安装")
}
func InitializeCLI(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 0 && (args[0] == "status" || args[0] == "log") {
		return shareServiceCLI(ctx, args[0], args[1:], out)
	}
	p := flag.NewFlagSet("project-alpha share-node", flag.ContinueOnError)
	p.SetOutput(out)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  sudo project-alpha share-node --listen-host IP --control-url URL [--control-key-file FILE] [选项]
  sudo project-alpha share-node --add-control-key "ssh-ed25519 AAAA…"
  sudo project-alpha share-node --add-control-file FILE
  sudo project-alpha share-node --uninstall [--no-reload] [--no-service]
  project-alpha share-node status
  project-alpha share-node log [-n 行数] [-f]

在 share node 本机初始化 alpha-worker、alpha-jump、sshd 和 HTTP 代理服务。
初始化完成后退出；代理以 alpha-worker 运行，由 systemd 持续托管并开机启动。
管理公钥可在初始化时提供，也可稍后用 --add-control-key / --add-control-file 追加。
未配置管理公钥时 alpha-worker 不接受 SSH 登录。总控授权后管理成员公钥。
成员使用 alpha-jump 转发 SSH，通过监听 IP 和入口端口访问总控网页。
status 查看 HTTP 代理的 systemd 状态；log 默认显示最近 50 行日志，-f 持续跟踪。
查看日志需要 journal 读取权限；权限不足时使用 sudo。
使用 project-alpha share-node <子命令> --help 查看详细用法。

示例：
  sudo project-alpha share-node --control-key-file /tmp/control-service.pub \
    --listen-host 100.64.0.2 --control-url http://10.0.0.1:8765 --status-port 9765
  sudo project-alpha share-node --uninstall

非 systemd 部署：初始化时加 --no-service --no-reload，自行重载 sshd，
并以 alpha-worker 持续运行 project-alpha share-node --serve。

选项：
`)
		p.PrintDefaults()
	}
	keyPath := p.String("control-key-file", "", "可选：初始化时添加的总控 SSH 公钥文件（一行公钥）")
	var additions controlKeyFlags
	p.Func("add-control-key", "追加一行总控 SSH 公钥，可重复使用", additions.addKey)
	p.Func("add-control-file", "从文件读取并追加一行总控 SSH 公钥，可重复使用", additions.addFile)
	host := p.String("listen-host", "", "share node 的 Tailscale IP")
	port := p.Int("status-port", 8765, "share node 总控网页入口端口")
	noReload := p.Bool("no-reload", false, "校验配置后由管理员重载 sshd")
	noService := p.Bool("no-service", false, "安装代理服务单元，但不调用 systemd 启停；由管理员运行代理")
	controlURL := p.String("control-url", "", "share node 可访问的总控内网 HTTP/HTTPS 地址")
	serve := p.Bool("serve", false, "以 alpha-worker 运行已安装的 HTTP 代理")
	uninstall := p.Bool("uninstall", false, "撤销 SSH 配置，删除两个专用账号、工具和公钥数据；会停止 HTTP 代理；需先断开成员及管理 SSH 连接")
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("不接受额外参数")
	}
	var invalidFlag string
	p.Visit(func(f *flag.Flag) {
		if *serve && f.Name != "serve" || *uninstall && f.Name != "uninstall" && f.Name != "no-reload" && f.Name != "no-service" {
			invalidFlag = f.Name
		}
	})
	if invalidFlag != "" {
		return fmt.Errorf("当前操作不接受 --%s", invalidFlag)
	}
	if *serve {
		return serveShareProxy(ctx, systemKeyStore())
	}
	if *uninstall {
		if os.Geteuid() != 0 {
			return fmt.Errorf("创建或删除系统账号及修改 sshd 需要 sudo")
		}
		unlock, err := lockShareInstall()
		if err != nil {
			return err
		}
		defer unlock()
		return uninstallShare(ctx, *noReload, *noService, out)
	}
	keys := []string{}
	if *keyPath != "" {
		if err := additions.addFile(*keyPath); err != nil {
			return err
		}
	}
	keys = mergeControlKeys(keys, additions.keys)
	addOnly := len(additions.keys) > 0 && *keyPath == "" && *host == "" && *controlURL == ""
	if addOnly {
		p.Visit(func(f *flag.Flag) {
			if f.Name != "add-control-key" && f.Name != "add-control-file" {
				invalidFlag = f.Name
			}
		})
		if invalidFlag != "" {
			return fmt.Errorf("仅追加管理公钥时不接受 --%s", invalidFlag)
		}
		if os.Geteuid() != 0 {
			return fmt.Errorf("追加管理公钥需要 sudo")
		}
		unlock, err := lockShareInstall()
		if err != nil {
			return err
		}
		defer unlock()
		if err = appendControlKeys(systemKeyStore(), keys, atomicRootFile); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "管理公钥已添加（重复公钥自动忽略），无需重启服务。")
		return err
	}
	if *host == "" || *controlURL == "" || *port < 1024 || *port > 65535 {
		return fmt.Errorf("初始化需提供 --listen-host、--control-url 和 1024–65535 的 --status-port；管理公钥选填")
	}
	ip, err := platform.InternalIP(*host)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("创建系统账号及修改 sshd 需要 sudo；日常 SSH 管理使用 alpha-worker")
	}
	target, err := proxyTarget(*controlURL)
	if err != nil {
		return err
	}
	if target.Host == netAddress(ip, *port) {
		return fmt.Errorf("总控代理目标不能指向分享节点自身的入口")
	}

	unlock, err := lockShareInstall()
	if err != nil {
		return err
	}
	defer unlock()
	return initializeShare(ctx, installation{Version: installationFormat, ControlKeys: keys, ControlURL: *controlURL, ListenHost: ip, StatusPort: *port}, *noReload, *noService, out)
}
func initializeShare(ctx context.Context, c installation, noReload, noService bool, out io.Writer) (result error) {
	unit, err := sshUnit(ctx, noReload)
	if err != nil {
		return err
	}
	existing := false
	if _, err = os.Lstat(installationDirectory); err == nil {
		r, old, e := systemKeyStore().open()
		if e != nil {
			return fmt.Errorf("已有安装格式无效，原数据保留，请使用新的安装环境；--uninstall 仅支持具有完整 alpha-jump/alpha-worker 身份记录的安装: %w", e)
		}
		r.Close()
		c.ControlKeys = mergeControlKeys(old.ControlKeys, c.ControlKeys)
		existing = true
		c.JumpUID, c.JumpGID, c.WorkerUID, c.WorkerGID = old.JumpUID, old.JumpGID, old.WorkerUID, old.WorkerGID
	} else if !os.IsNotExist(err) {
		return err
	}
	if !existing {
		for _, name := range []string{JumpUser, WorkerUser} {
			if _, e := user.Lookup(name); e == nil {
				return fmt.Errorf("%s 已存在，拒绝覆盖未管理账号", name)
			} else if _, ok := e.(user.UnknownUserError); !ok {
				return e
			}
			if _, e := user.LookupGroup(name); e == nil {
				return fmt.Errorf("%s 组已存在，拒绝覆盖", name)
			} else if _, ok := e.(user.UnknownGroupError); !ok {
				return e
			}
		}
	}
	if previous, _, e := rootFile(proxyUnitPath); e == nil {
		if !existing || !bytes.Equal(previous, proxyUnit()) {
			return fmt.Errorf("代理服务单元已存在且不属于当前安装，拒绝覆盖")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	created := []string{}
	type savedFile struct {
		path   string
		raw    []byte
		mode   os.FileMode
		exists bool
	}
	saved := []savedFile{}
	sshChanged, serviceChanged := false, false
	write := func(path string, raw []byte, mode os.FileMode) error {
		previous, perm, e := rootFile(path)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		saved = append(saved, savedFile{path, previous, perm, e == nil})
		return atomicRootFile(path, raw, mode)
	}
	defer func() {
		if result == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		recordError := func(action string, err error) {
			if err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, fmt.Errorf("%s 失败: %w", action, err))
			}
		}
		run := func(name string, args ...string) {
			_, err := installRun(rollbackCtx, name, args...)
			recordError("回滚 "+name+" "+strings.Join(args, " "), err)
		}
		for i := len(saved) - 1; i >= 0; i-- {
			v := saved[i]
			var e error
			if v.exists {
				e = atomicRootFile(v.path, v.raw, v.mode)
			} else {
				e = os.Remove(v.path)
			}
			recordError("恢复 "+v.path, e)
		}
		if sshChanged {
			recordError("恢复 sshd 重载", reloadSSH(rollbackCtx, unit))
		}
		if serviceChanged {
			if existing {
				run("systemctl", "daemon-reload")
				run("systemctl", "restart", proxyUnitName)
			} else {
				run("systemctl", "disable", "--now", proxyUnitName)
				run("systemctl", "daemon-reload")
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			run("/usr/sbin/userdel", created[i])
			if _, e := user.LookupGroup(created[i]); e == nil {
				run("/usr/sbin/groupdel", created[i])
			} else if _, ok := e.(user.UnknownGroupError); !ok {
				recordError("查询 "+created[i]+" 组", e)
			}
		}
		if !existing {
			recordError("清理公钥目录", os.Remove(filepath.Join(installationDirectory, "keys")))
			recordError("清理安装目录", os.Remove(installationDirectory))
			for _, name := range created {
				recordError("清理 "+name+" home", os.Remove("/var/empty/"+name))
			}
		}
	}()
	if !existing {
		for _, name := range []string{JumpUser, WorkerUser} {
			home, shell := jumpHome, "/bin/false"
			if name == WorkerUser {
				home, shell = workerHome, "/bin/sh"
			}
			if err = rootDirectory(home, 0755); err != nil {
				return err
			}
			args := []string{"--system", "--user-group", "--home-dir", home, "--no-create-home", "--shell", shell, "--password", "*"}
			if name == WorkerUser {
				args = append(args, "--groups", JumpUser)
			}
			args = append(args, name)
			if _, err = installRun(ctx, "/usr/sbin/useradd", args...); err != nil {
				return err
			}
			created = append(created, name)
			u, err := user.Lookup(name)
			if err != nil {
				return err
			}
			uid, e := strconv.Atoi(u.Uid)
			if e != nil {
				return e
			}
			gid, e := strconv.Atoi(u.Gid)
			if e != nil {
				return e
			}
			if name == JumpUser {
				c.JumpUID, c.JumpGID = uid, gid
			} else {
				c.WorkerUID, c.WorkerGID = uid, gid
			}
		}
	}
	if err = rootDirectory(installationDirectory, 0755); err != nil {
		return err
	}
	keys := filepath.Join(installationDirectory, "keys")
	if err = os.Mkdir(keys, 0700); err == nil {
		if err = os.Chown(keys, c.WorkerUID, c.JumpGID); err != nil {
			return err
		}
		if err = os.Chmod(keys, os.ModeSetgid|0750); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	r, err := os.OpenRoot(installationDirectory)
	if err != nil {
		return err
	}
	k, err := openKeys(r, c)
	r.Close()
	if err != nil {
		return err
	}
	unlockKeys, err := lockKeyFiles(k, c)
	if err != nil {
		k.Close()
		return err
	}
	defer k.Close()
	defer unlockKeys()
	_, err = loadSnapshot(k, c)
	if err != nil {
		return err
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err = rootDirectory(filepath.Dir(readerExecutable), 0755); err != nil {
		return err
	}
	original, mode, err := rootFile(sshConfigPath)
	if err != nil {
		return err
	}
	candidate, err := configuredSSH(original)
	if err != nil {
		return err
	}
	if err = write(readerExecutable, binary, 0755); err != nil {
		return err
	}
	if err = validateSSH(ctx, candidate, true); err != nil {
		return err
	}
	manifest, err := marshalInstallation(c)
	if err != nil {
		return err
	}
	if err = write(filepath.Join(installationDirectory, "installation.json"), manifest, 0644); err != nil {
		return err
	}
	if err = write(workerKeysPath, controlAuthorizedKeys(c.ControlKeys), 0644); err != nil {
		return err
	}
	if err = write(proxyUnitPath, proxyUnit(), 0644); err != nil {
		return err
	}
	backup := sshConfigPath + ".before-alpha-share-node"
	if _, err = os.Lstat(backup); os.IsNotExist(err) {
		if err = atomicRootFile(backup, original, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err = write(sshConfigPath, candidate, mode); err != nil {
		return err
	}
	sshChanged = true
	if err = reloadSSH(ctx, unit); err != nil {
		return fmt.Errorf("重载失败，撤销本次安装: %w", err)
	}
	if !noService {
		serviceChanged = true
		if _, err = installRun(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if _, err = installRun(ctx, "systemctl", "enable", proxyUnitName); err != nil {
			return err
		}
		if _, err = installRun(ctx, "systemctl", "restart", proxyUnitName); err != nil {
			return err
		}
	}
	if len(c.ControlKeys) == 0 {
		fmt.Fprintln(out, "尚未添加管理公钥；请用 --add-control-key 或 --add-control-file 授权总控后再加入分享池。")
	}
	_, err = fmt.Fprintf(out, "share node 已初始化：alpha-worker 负责内部命令及 HTTP 代理，alpha-jump 负责成员访问；总控网页入口 %s -> %s。\n", netAddress(c.ListenHost, c.StatusPort), c.ControlURL)
	return err
}
func uninstallShare(ctx context.Context, noReload, noService bool, out io.Writer) (result error) {
	_, err := os.Lstat(installationDirectory)
	if os.IsNotExist(err) {
		for _, name := range []string{JumpUser, WorkerUser} {
			if _, e := user.Lookup(name); e == nil {
				return fmt.Errorf("缺失安装记录但 %s 仍存在，拒绝删除未核对账号", name)
			} else if _, ok := e.(user.UnknownUserError); !ok {
				return e
			}
		}
		_, err = fmt.Fprintln(out, "share node 未安装。")
		return err
	}
	if err != nil {
		return err
	}
	s := systemKeyStore()
	r, c, err := s.openUninstallManifest()
	if err != nil {
		return err
	}
	defer r.Close()
	hasUnit := false
	if raw, _, e := rootFile(proxyUnitPath); e == nil {
		if !bytes.Equal(raw, proxyUnit()) {
			return fmt.Errorf("代理服务单元已改变，拒绝卸载")
		}
		hasUnit = true
	} else if !os.IsNotExist(e) {
		return e
	}
	wasActive := false
	if !noService && hasUnit {
		if _, e := installRun(ctx, "systemctl", "is-active", "--quiet", proxyUnitName); e == nil {
			wasActive = true
		}
		if _, e := installRun(ctx, "systemctl", "stop", proxyUnitName); e != nil {
			return e
		}
	}
	defer func() {
		if result != nil && wasActive {
			if _, e := user.Lookup(WorkerUser); e == nil {
				_, _ = installRun(context.WithoutCancel(ctx), "systemctl", "start", proxyUnitName)
			}
		}
	}()
	for name, ids := range map[string][2]int{JumpUser: {c.JumpUID, c.JumpGID}, WorkerUser: {c.WorkerUID, c.WorkerGID}} {
		home := "/var/empty/" + name
		if info, e := os.Lstat(home); e == nil {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Uid != 0 || info.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("%s home 身份或权限无效，拒绝卸载", name)
			}
			entries, e := os.ReadDir(home)
			if e != nil {
				return e
			}
			if len(entries) > 0 {
				return fmt.Errorf("%s home 包含额外文件，保留数据并拒绝卸载", name)
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		if g, e := user.LookupGroup(name); e == nil {
			if g.Gid != strconv.Itoa(ids[1]) {
				return fmt.Errorf("%s 组身份已改变，拒绝卸载", name)
			}
		} else if _, ok := e.(user.UnknownGroupError); !ok {
			return e
		}

		u, e := user.Lookup(name)
		if e != nil {
			if _, ok := e.(user.UnknownUserError); ok {
				continue
			}
			return e
		}
		if u.Uid != strconv.Itoa(ids[0]) || u.Gid != strconv.Itoa(ids[1]) || u.HomeDir != "/var/empty/"+name {
			return fmt.Errorf("%s 身份已改变，拒绝卸载", name)
		}
		cmd := exec.CommandContext(ctx, "pgrep", "-u", u.Uid)
		if e = cmd.Run(); e == nil {
			return fmt.Errorf("%s 仍有进程，请先断开管理及成员 SSH 连接后卸载", name)
		} else if exit, ok := e.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return fmt.Errorf("无法检查 %s 的进程: %w", name, e)
		}
	}
	entries, err := rootEntries(r)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "installation.json" && entry.Name() != "worker_authorized_keys" && entry.Name() != "keys" {
			return fmt.Errorf("安装目录包含未知文件 %s，拒绝清理", entry.Name())
		}
	}
	k, err := openKeys(r, c)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var keyEntries []os.DirEntry
	if k != nil {
		defer k.Close()
		keyEntries, err = uninstallKeyEntries(k, c)
		if err != nil {
			return err
		}
	}
	if _, _, err = rootFile(readerExecutable); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, _, err = rootFile(workerKeysPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	unit, err := sshUnit(ctx, noReload)
	if err != nil {
		return err
	}
	original, mode, err := rootFile(sshConfigPath)
	if err != nil {
		return err
	}
	candidate, err := removeManagedSSH(original)
	if err != nil {
		return err
	}
	if err = validateSSH(ctx, candidate, false); err != nil {
		return err
	}
	if err = atomicRootFile(sshConfigPath, candidate, mode); err != nil {
		return err
	}
	if err = reloadSSH(ctx, unit); err != nil {
		restore := atomicRootFile(sshConfigPath, original, mode)
		_ = reloadSSH(context.WithoutCancel(ctx), unit)
		return fmt.Errorf("卸载重载失败，SSH 配置已恢复（恢复错误 %v）: %w", restore, err)
	}
	if !noService && hasUnit {
		if _, err = installRun(ctx, "systemctl", "disable", proxyUnitName); err != nil {
			return err
		}
	}
	for _, name := range []string{WorkerUser, JumpUser} {
		if _, err = user.Lookup(name); err == nil {
			if _, err = installRun(ctx, "/usr/sbin/userdel", name); err != nil {
				return err
			}
		} else if _, ok := err.(user.UnknownUserError); !ok {
			return err
		}
		if g, e := user.LookupGroup(name); e == nil {
			gid := c.JumpGID
			if name == WorkerUser {
				gid = c.WorkerGID
			}
			if g.Gid != strconv.Itoa(gid) {
				return fmt.Errorf("%s 组身份已改变，拒绝删除", name)
			}
			if _, err = installRun(ctx, "/usr/sbin/groupdel", name); err != nil {
				return err
			}
		} else if _, ok := e.(user.UnknownGroupError); !ok {
			return e
		}
	}
	for _, entry := range keyEntries {
		if err = k.Remove(entry.Name()); err != nil {
			return err
		}
	}
	if k != nil {
		k.Close()
	}
	if err = r.Remove("keys"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = r.Remove("worker_authorized_keys"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = os.Remove(readerExecutable); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = os.Remove(proxyUnitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !noService {
		if _, err = installRun(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	for _, home := range []string{workerHome, jumpHome} {
		if err = os.Remove(home); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("账号和 SSH 配置已撤销；home 非空或无法删除，保留 %s: %w", home, err)
		}
	}
	// Keep the manifest until all other cleanup succeeds so a retry can verify identities.
	if err = r.Remove("installation.json"); err != nil {
		return err
	}
	r.Close()
	if err = os.Remove(installationDirectory); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "已撤销 share node HTTP 代理服务及 SSH 配置，删除 alpha-worker、alpha-jump、工具和公钥数据；sshd 原始备份保留。")
	return err
}

// Removal needs recorded identities, not a runnable installation. Never migrate
// or rewrite the manifest, and never infer missing IDs from existing accounts.
func (s keyStore) openUninstallManifest() (*os.Root, installation, error) {
	var c installation
	r, raw, err := s.readManifest()
	if err != nil {
		return nil, c, err
	}
	var fields map[string]json.RawMessage
	err = strictJSON(raw, &fields)
	if err == nil {
		for _, field := range []struct {
			name string
			id   *int
		}{{"jump_uid", &c.JumpUID}, {"jump_gid", &c.JumpGID}, {"worker_uid", &c.WorkerUID}, {"worker_gid", &c.WorkerGID}} {
			if e := json.Unmarshal(fields[field.name], field.id); e != nil || *field.id <= 0 {
				err = fmt.Errorf("%s 必须为正整数", field.name)
				break
			}
		}
	}
	if err == nil && c.WorkerUID == c.JumpUID {
		err = fmt.Errorf("两个专用账号的 UID 不能相同")
	}
	if err != nil {
		r.Close()
		return nil, c, fmt.Errorf("安装记录缺少有效的双账号身份，保留数据并拒绝卸载；需人工核对并备份清理原安装，不能用服务用户替代专用账号: %w", err)
	}
	return r, c, nil
}

// Explicit uninstall removes managed key files without interpreting their data.
func uninstallKeyEntries(k *os.Root, c installation) ([]os.DirEntry, error) {
	entries, err := rootEntries(k)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() != "keys.json" && entry.Name() != ".lock" {
			return nil, fmt.Errorf("公钥目录包含未知文件 %s，拒绝清理", entry.Name())
		}
		info, err := k.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || int(st.Uid) != c.WorkerUID || int(st.Gid) != c.JumpGID || info.Mode().Perm()&0037 != 0 {
			return nil, fmt.Errorf("公钥文件身份或权限无效，拒绝清理")
		}
	}
	return entries, nil
}

func rootEntries(r *os.Root) ([]os.DirEntry, error) {
	f, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func lockShareInstall() (func(), error) {
	f, err := os.OpenFile("/run/project-alpha-share-node.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || st.Uid != 0 || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, fmt.Errorf("share node 安装锁无效")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("share node 正在安装或卸载，请稍后重试")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
