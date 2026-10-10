package rootless

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"
)

type daemonInfo struct {
	SecurityOptions []string
	DockerRootDir   string
}

func isRootless(info daemonInfo) bool {
	for _, s := range info.SecurityOptions {
		if strings.Contains(s, "rootless") {
			return true
		}
	}
	return false
}
func (m *manager) verifyDaemon(u account) error {
	r, err := m.docker(u, "", "info", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	var info daemonInfo
	if err = json.Unmarshal([]byte(r.Out), &info); err != nil {
		return err
	}
	if !isRootless(info) {
		return errors.New("socket 对应的 daemon 不是 rootless，拒绝继续。")
	}
	if info.DockerRootDir != layout(u).Data {
		return errors.New("daemon 的数据目录不匹配，拒绝继续。")
	}
	return nil
}
func checkDependencies() error {
	var missing []string
	for _, name := range []string{"docker", "dockerd", "dockerd-rootless.sh", "rootlesskit", "newuidmap", "newgidmap", "slirp4netns", "iptables", "systemctl", "loginctl", "runuser", "useradd", "usermod", "flock", "getent"} {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少依赖：%s\nDebian/Ubuntu（已配置 Docker 官方软件源）：\n  sudo apt-get install uidmap dbus-user-session slirp4netns iptables docker-ce-rootless-extras\n工具不会自动安装软件包或改动系统 Docker 服务。", strings.Join(missing, ", "))
	}
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("daemon 需要在运行 systemd 的 Linux 宿主机执行。")
	}
	return nil
}
func (m *manager) initialize(o options) error {
	if err := checkDependencies(); err != nil {
		return err
	}
	if _, err := user.Lookup(accountName); err != nil {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			return err
		}
		home := "/home/" + accountName
		if _, err = os.Lstat(home); err == nil {
			return fmt.Errorf("用户不存在但家目录已存在，拒绝接管：%s", home)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err = m.run([]string{"useradd", "--create-home", "--home-dir", home, "--user-group", "--shell", "/usr/sbin/nologin", accountName}, nil, 60*time.Second, true); err != nil {
			return err
		}
		if err = os.Chmod(home, 0700); err != nil {
			return err
		}
	}
	u, err := lookupAccount()
	if err != nil {
		return err
	}
	// Validate the installed executable as the service user before stopping
	// any existing service (e.g. noexec mounts or restrictive parent modes).
	if _, err = m.userRun(u, []string{supervisorPath, "--help"}, 10*time.Second, true); err != nil {
		return fmt.Errorf("supervisor 不可执行，原服务未停止：%w", err)
	}
	lock, err := lockHome(u)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = m.configureSubids(u); err != nil {
		return err
	}
	// The daemon owns startup. Disable independent boot startup before replacing
	// the managed unit; an existing data directory is always retained.
	if data, e := os.ReadFile(layout(u).Unit); e == nil {
		if !strings.HasPrefix(string(data), managed) {
			return fmt.Errorf("拒绝接管非本工具服务：%s", layout(u).Unit)
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if _, err = m.run([]string{"loginctl", "enable-linger", accountName}, nil, 60*time.Second, true); err != nil {
		return err
	}
	if _, err = m.run([]string{"systemctl", "start", fmt.Sprintf("user@%d.service", u.UID)}, nil, 60*time.Second, true); err != nil {
		return err
	}
	if _, err = os.Stat(layout(u).Unit); err == nil {
		if _, err = m.systemctl(u, "disable", "--now", serviceName); err != nil {
			return err
		}
	}
	if err = m.worker(workerRequest{Action: "prepare", User: u, Options: o}, true); err != nil {
		return err
	}
	p := layout(u)
	if _, err = m.userRun(u, []string{"dockerd", "--validate", "--config-file", p.Config + "/daemon.json"}, 60*time.Second, true); err != nil {
		return err
	}
	if _, err = m.userRun(u, []string{"env", "TMPDIR=" + p.Tmp, "rootlesskit", "true"}, 60*time.Second, true); err != nil {
		return err
	}
	_, err = m.systemctl(u, "daemon-reload")
	return err
}
func (m *manager) configureProxy(u account, o options) error {
	p := layout(u)
	for _, path := range []string{p.Unit, p.Config + "/daemon.json"} {
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return errors.New("请先启动 rootless-docker daemon。")
		}
	}
	lock, err := lockHome(u)
	if err != nil {
		return err
	}
	defer lock.Close()
	if len(o.Proxies) == 0 && !o.Clear && o.AllowLoopback == nil {
		return m.worker(workerRequest{Action: "show-proxy", User: u}, true)
	}
	if err = m.worker(workerRequest{Action: "prepare", User: u, Options: o}, true); err != nil {
		return err
	}
	if _, err = m.systemctl(u, "daemon-reload"); err != nil {
		return err
	}
	_, err = m.systemctl(u, "restart", serviceName)
	if err == nil {
		err = m.verifyDaemon(u)
	}
	if err != nil {
		fmt.Fprintf(m.errOut, "代理配置已保存，但服务启动/验证失败；请查看 %s/dockerd.log。\n", p.Log)
		return err
	}
	action := "更新"
	if o.Clear {
		action = "清除"
	}
	fmt.Fprintf(m.out, "代理配置已%s，rootless dockerd 已重启。\n", action)
	return nil
}

type containerState struct {
	Pid                         int
	Running, Paused, Restarting bool
	StartedAt                   string
}
type containerInfo struct {
	Id    string
	State containerState
}

func (m *manager) inspect(u account, host string, args ...string) (containerInfo, error) {
	r, err := m.docker(u, host, append([]string{"container", "inspect"}, args...)...)
	if err != nil {
		return containerInfo{}, err
	}
	var items []containerInfo
	if err = json.Unmarshal([]byte(r.Out), &items); err != nil {
		return containerInfo{}, err
	}
	if len(items) != 1 {
		return containerInfo{}, errors.New("Docker inspect 必须返回一个容器")
	}
	return items[0], nil
}
func socketIdentity(path string) (syscall.Stat_t, error) {
	var s syscall.Stat_t
	err := syscall.Stat(path, &s)
	return s, err
}
