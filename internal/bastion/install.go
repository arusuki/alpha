package bastion

import (
	"bytes"
	"context"
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

	"project-alpha/internal/sshkeys"
)

const sshConfigPath = "/etc/ssh/sshd_config"
const jumpHome = "/var/empty/alpha-jump"
const configBegin = "# BEGIN project-alpha alpha-jump"
const configEnd = "# END project-alpha alpha-jump"

// These restrictions are set by the root-owned SSH configuration, independently
// of the key publisher. The publisher cannot grant shell access or authorize a
// different system account by changing its output.
func jumpSSHConfig() string {
	return configBegin + `
Match User alpha-jump
    AuthorizedKeysFile none
    AuthorizedKeysCommand /usr/local/libexec/project-alpha-jump bastion authorized-keys %u %U
    AuthorizedKeysCommandUser alpha-jump
    AuthenticationMethods publickey
    PubkeyAuthentication yes
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowTcpForwarding local
    AllowStreamLocalForwarding no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTTY no
    PermitTunnel no
    PermitUserRC no
    MaxSessions 0
    ForceCommand /bin/false
` + configEnd + "\n"
}

func configuredSSH(original []byte) ([]byte, error) {
	s := string(original)
	if strings.Contains(s, configBegin) || strings.Contains(s, configEnd) {
		if strings.Count(s, configBegin) != 1 || strings.Count(s, configEnd) != 1 {
			return nil, fmt.Errorf("sshd_config 的 alpha-jump 标记无效")
		}
		start, end := strings.Index(s, configBegin), strings.Index(s, configEnd)
		if end < start || strings.TrimSpace(s[end+len(configEnd):]) != "" {
			return nil, fmt.Errorf("alpha-jump 配置必须位于 sshd_config 末尾，请核对已有配置")
		}
		s = s[:start]
	}
	return []byte(strings.TrimRight(s, "\n") + "\n\n" + jumpSSHConfig()), nil
}

func CLI(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 3 && args[0] == "authorized-keys" {
		return systemKeyStore().authorizedKeys(args[1], args[2], out)
	}
	if len(args) == 0 || !validAccountAction(args[0]) {
		return fmt.Errorf("用法: project-alpha bastion <init|adopt|release|delete> --service-user <普通服务用户> --data-dir <总控目录> [--confirm alpha-jump]")
	}
	p := flag.NewFlagSet("bastion "+args[0], flag.ContinueOnError)
	service := p.String("service-user", "", "运行 control 的现有非 root 用户")
	directory := p.String("data-dir", "", "绑定的 control 数据目录；新目录以服务用户初始化")
	confirm := p.String("confirm", "", "删除账号时须填写 alpha-jump；保留工具和 data")
	noReload := p.Bool("no-reload", false, "仅安装和校验配置，由管理员另行重载 sshd")
	if err := p.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 || *service == "" || *directory == "" {
		return fmt.Errorf("必须指定 --service-user 和 --data-dir")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("初始化需要 sudo；日常运行 control 不需要 sudo")
	}
	if args[0] == "delete" && *confirm != JumpUser {
		return fmt.Errorf("删除账号须指定 --confirm alpha-jump；工具和 data 将保留")
	}
	return install(ctx, args[0], *service, *directory, *noReload, out)
}

func runInstall(ctx context.Context, name string, args ...string) ([]byte, error) {
	child, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(child, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func numericAccount(u *user.User) (int, int, error) {
	uid, e1 := strconv.Atoi(u.Uid)
	gid, e2 := strconv.Atoi(u.Gid)
	if e1 != nil || e2 != nil || uid <= 0 || gid <= 0 {
		return 0, 0, fmt.Errorf("必须使用非 root 系统账号")
	}
	return uid, gid, nil
}

// The root installer never opens the control database. A child running with the
// service user's UID, GID and groups initializes/checks it using normal rules.
func prepareControl(ctx context.Context, u *user.User, directory, executable, operation string) (string, error) {
	uid, gid, err := numericAccount(u)
	if err != nil {
		return "", err
	}
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return "", err
	}
	defer parent.Close()
	name := filepath.Base(directory)
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		if err = parent.Mkdir(name, 0700); err != nil {
			return "", err
		}
		// Pin the new directory before changing ownership. A writable parent
		// must never turn root's chown into a symlink to a different file.
		created, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return "", err
		}
		err = created.Chown(uid, gid)
		created.Close()
		if err != nil {
			return "", err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(st.Uid) != uid || info.Mode().Perm()&0022 != 0 {
		return "", fmt.Errorf("总控目录必须属于服务用户且不能允许其他用户写入")
	}
	groups, err := u.GroupIds()
	if err != nil {
		return "", err
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	for _, group := range groups {
		v, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			return "", err
		}
		cred.Groups = append(cred.Groups, uint32(v))
	}
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(child, executable, "bastion", operation, directory)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("以 %s 核验总控目录或同步公钥失败: %w: %s", u.Username, err, stderr.String())
	}
	id := strings.TrimSpace(string(raw))
	if !sshkeys.ID.MatchString(id) {
		return "", fmt.Errorf("总控返回了无效实例 ID")
	}
	return id, nil
}

func rootDirectory(path string, mode os.FileMode) error {
	if path == "/" {
		return nil
	}
	if err := rootDirectory(filepath.Dir(path), 0755); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err = os.Mkdir(path, mode); err != nil {
			return err
		}
		if err = os.Chmod(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(st.Uid) != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s 必须是 root 持有且其他用户不可写的普通目录", path)
	}
	return nil
}

func rootFile(path string, raw []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || int(st.Uid) != 0 || st.Nlink != 1 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("拒绝覆盖非 root 普通文件 %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".alpha-install-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Chown(0, 0)
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

func checkEffectiveSSH(raw []byte) error {
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	for _, line := range strings.Split(jumpSSHConfig(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "#" || fields[0] == "Match" {
			continue
		}
		key, want := strings.ToLower(fields[0]), strings.Join(fields[1:], " ")
		if values[key] != want {
			return fmt.Errorf("已有 SSH 配置覆盖了 %s（实际 %q，要求 %q），请先解决冲突", fields[0], values[key], want)
		}
	}
	return nil
}

func install(ctx context.Context, action, service, directory string, noReload bool, out io.Writer) error {
	u, err := user.Lookup(service)
	if err != nil {
		return fmt.Errorf("服务用户不存在: %w", err)
	}
	uid, _, err := numericAccount(u)
	if err != nil || service == JumpUser {
		return fmt.Errorf("control 必须使用不同于 alpha-jump 的非 root 用户")
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	sshd, unit := "/usr/sbin/sshd", ""
	if action == "init" || action == "adopt" {
		if _, err = runInstall(ctx, sshd, "-t"); err != nil {
			return fmt.Errorf("请先安装并配置可用的 OpenSSH 服务: %w", err)
		}
		if !noReload {
			for _, candidate := range []string{"ssh.service", "sshd.service"} {
				if _, e := runInstall(ctx, "/usr/bin/systemctl", "is-active", "--quiet", candidate); e == nil {
					unit = candidate
					break
				}
			}
			if unit == "" {
				return fmt.Errorf("未找到活动的 ssh/sshd systemd 服务；自行管理 sshd 时使用 --no-reload 并在安装后重载")
			}
		}
	}
	id, err := prepareControl(ctx, u, directory, executable, "prepare-control")
	if err != nil {
		return err
	}
	if err = rootDirectory(installationDirectory, 0755); err != nil {
		return err
	}
	// Serialize installers independently of normal key publication.
	lock, err := os.OpenFile(filepath.Join(installationDirectory, ".install.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("另一初始化正在运行")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var c installation
	manifest := filepath.Join(installationDirectory, "installation.json")
	if _, err = os.Lstat(manifest); err == nil {
		open := systemKeyStore().open
		if action == "adopt" {
			open = systemKeyStore().openForAdoption
		}
		r, existing, err := open()
		if err != nil {
			return err
		}
		r.Close()
		c = existing
		if err = checkInstallationBinding(c, id, service, uid, action); err != nil {
			return err
		}
		if c.JumpUID == uid {
			return fmt.Errorf("control 服务用户不能与 alpha-jump 使用相同 UID")
		}
	} else if os.IsNotExist(err) {
		if action == "release" || action == "delete" {
			return fmt.Errorf("alpha-jump 尚未由当前 control 接管，请先接管")
		}
		if _, e := os.Lstat(filepath.Join(installationDirectory, "keys")); !os.IsNotExist(e) {
			return fmt.Errorf("已有公钥 data 但缺少安装记录，拒绝覆盖；请核对数据，格式不匹配时使用新数据目录")
		}
		jump, e := user.Lookup(JumpUser)
		if action == "adopt" {
			if e != nil {
				return fmt.Errorf("接管要求已有 alpha-jump 账号: %w", e)
			}
		} else {
			if e == nil {
				return fmt.Errorf("alpha-jump 已存在，请选择接管已有账号")
			}
			if _, absent := e.(user.UnknownUserError); !absent {
				return e
			}
			if _, e = user.LookupGroup(JumpUser); e == nil {
				return fmt.Errorf("alpha-jump 组已存在，请核对后添加账号并选择接管")
			} else if _, absent := e.(user.UnknownGroupError); !absent {
				return e
			}
			if err = rootDirectory(jumpHome, 0755); err != nil {
				return err
			}
			if _, err = runInstall(ctx, "/usr/sbin/useradd", "--system", "--user-group", "--home-dir", jumpHome, "--no-create-home", "--shell", "/bin/false", "--password", "*", JumpUser); err != nil {
				return err
			}
			jump, err = user.Lookup(JumpUser)
			if err != nil {
				return err
			}
		}
		juid, jgid, err := numericAccount(jump)
		if err != nil || juid == uid {
			return fmt.Errorf("alpha-jump 必须为独立的非 root 系统账号")
		}
		c = installation{Version: keyFormat, ControlID: id, ServiceUser: service, ServiceUID: uid, JumpUID: juid, JumpGID: jgid}
		if err = saveInstallation(c); err != nil {
			return err
		}
	} else {
		return err
	}
	keys := filepath.Join(installationDirectory, "keys")
	if err = os.Mkdir(keys, 0700); err == nil {
		if err = os.Chown(keys, c.ServiceUID, c.JumpGID); err != nil {
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
	defer k.Close()
	keyLock, err := lockInstallationKeys(k, c)
	if err != nil {
		return err
	}
	defer keyLock.Close()
	snapshot, err := loadSnapshot(k, c)
	if err != nil {
		return err
	}
	if action == "release" || action == "delete" {
		return manageAccount(ctx, action, c, snapshot, out)
	}
	if c.AccountRemoved {
		if err = recreateAccount(ctx, c); err != nil {
			return err
		}
		c.AccountRemoved = false
		if err = saveInstallation(c); err != nil {
			return err
		}
	}

	if err = rootDirectory(filepath.Dir(readerExecutable), 0755); err != nil {
		return err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	if err = rootFile(readerExecutable, binary, 0755); err != nil {
		return err
	}
	if err = rootDirectory(filepath.Dir(sshConfigPath), 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(sshConfigPath))
	if err != nil {
		return err
	}
	original, err := readPrivateFile(root, filepath.Base(sshConfigPath), 0, -1, 0022, 1<<20)
	root.Close()
	if err != nil {
		return err
	}
	updated, err := configuredSSH(original)
	if err != nil {
		return err
	}
	candidate, err := os.CreateTemp(filepath.Dir(sshConfigPath), ".alpha-sshd-")
	if err != nil {
		return err
	}
	defer os.Remove(candidate.Name())
	_, err = candidate.Write(updated)
	closeErr := candidate.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if _, err = runInstall(ctx, sshd, "-t", "-f", candidate.Name()); err != nil {
		return err
	}
	effective, err := runInstall(ctx, sshd, "-T", "-f", candidate.Name(), "-C", "user=alpha-jump,host=localhost,addr=127.0.0.1")
	if err != nil {
		return err
	}
	if err = checkEffectiveSSH(effective); err != nil {
		return err
	}
	if !bytes.Equal(updated, original) {
		backup := sshConfigPath + ".before-alpha-jump"
		if _, e := os.Lstat(backup); os.IsNotExist(e) {
			if err = rootFile(backup, original, 0600); err != nil {
				return err
			}
		} else if e != nil {
			return e
		}
		if err = rootFile(sshConfigPath, updated, 0600); err != nil {
			return err
		}
	}
	if unit != "" {
		if _, err = runInstall(ctx, "/usr/bin/systemctl", "reload", unit); err != nil {
			if restore := rootFile(sshConfigPath, original, 0600); restore != nil {
				return fmt.Errorf("重载失败且恢复配置失败: %v; %w", err, restore)
			}
			_, reloadErr := runInstall(ctx, "/usr/bin/systemctl", "reload", unit)
			return fmt.Errorf("SSH 重载失败，已恢复原配置（恢复后重载: %v）: %w", reloadErr, err)
		}
	}
	if action == "adopt" && (c.ControlID != id || c.ServiceUID != uid || c.ServiceUser != service) {
		target := c
		target.ControlID, target.ServiceUser, target.ServiceUID = id, service, uid
		target.Ready, target.Released = true, false
		archive, err := transferInstallation(c, target, snapshot)
		if err != nil {
			return err
		}
		c = target
		fmt.Fprintf(out, "已将 alpha-jump 和全部现有公钥转交当前 control；原 data 完整保留在 %s。\n", archive)
	}
	if !c.Ready || c.Released {
		c.Ready = true
		c.Released = false
		if err = saveInstallation(c); err != nil {
			return err
		}
	}
	// The ordinary service user reconciles the pool with its own database.
	// Release the publisher lock first; the installation lock stays held.
	if err = keyLock.Close(); err != nil {
		return err
	}
	if _, err = prepareControl(ctx, u, directory, executable, "sync-control-keys"); err != nil {
		return fmt.Errorf("账号已接管，补齐当前用户公钥失败，可在公钥池重试同步: %w", err)
	}
	fmt.Fprintf(out, "alpha-jump 初始化完成；control 用户: %s；数据目录: %s\n后续以该普通用户运行 control，公钥管理无需 sudo。\n", service, directory)
	if noReload {
		fmt.Fprintln(out, "SSH 配置已校验但尚未重载，请在启用前重载 sshd。")
	}
	return nil
}
