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
		return errors.New("init 需要在运行 systemd 的 Linux 宿主机执行。")
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
	lock, err := lockHome(u)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = m.configureSubids(u); err != nil {
		return err
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
	if _, err = m.run([]string{"loginctl", "enable-linger", accountName}, nil, 60*time.Second, true); err != nil {
		return err
	}
	if _, err = m.run([]string{"systemctl", "start", fmt.Sprintf("user@%d.service", u.UID)}, nil, 60*time.Second, true); err != nil {
		return err
	}
	if _, err = m.systemctl(u, true, "daemon-reload"); err != nil {
		return err
	}
	if err = m.enableService(u, o); err != nil {
		fmt.Fprintf(m.errOut, "启动诊断：请查看 %s/dockerd.log\n", p.Log)
		return err
	}
	fmt.Fprintf(m.out, "已初始化 %s，开机自动启动。\n数据：%s\nSocket：%s\n", accountName, p.Data, p.Socket)
	if len(o.Proxies) > 0 || o.AllowLoopback != nil {
		fmt.Fprintln(m.out, "代理/网络配置已应用；已关联的容器请重新执行 add。")
	}
	return nil
}
func (m *manager) enableService(u account, o options) error {
	if len(o.Proxies) > 0 || o.AllowLoopback != nil {
		if _, err := m.systemctl(u, true, "enable", serviceName); err != nil {
			return err
		}
		if _, err := m.systemctl(u, true, "restart", serviceName); err != nil {
			return err
		}
	} else {
		if _, err := m.systemctl(u, true, "enable", "--now", serviceName); err != nil {
			return err
		}
	}
	return m.verifyDaemon(u)
}
func (m *manager) configureProxy(u account, o options) error {
	p := layout(u)
	for _, path := range []string{p.Unit, p.Config + "/daemon.json"} {
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return errors.New("请先执行 init，或使用 init --http-proxy/--https-proxy 初始化。")
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
	if _, err = m.systemctl(u, true, "daemon-reload"); err != nil {
		return err
	}
	_, err = m.systemctl(u, true, "restart", serviceName)
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
	fmt.Fprintf(m.out, "代理配置已%s，rootless daemon 已重启；已关联的容器请重新执行 add。\n", action)
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
func (m *manager) add(u account, o options) error {
	if err := m.verifyDaemon(u); err != nil {
		return err
	}
	r, err := m.docker(u, o.Host, "info", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	var rootful daemonInfo
	if err = json.Unmarshal([]byte(r.Out), &rootful); err != nil {
		return err
	}
	if isRootless(rootful) {
		return errors.New("add 的 --host 必须指向宿主机 rootful Docker。")
	}
	item, err := m.inspect(u, o.Host, "--", o.Container)
	if err != nil {
		return err
	}
	state := item.State
	if !state.Running || state.Paused || state.Restarting || state.Pid <= 1 {
		return errors.New("目标容器必须处于运行状态，且不能暂停或正在重启。")
	}
	mapping, err := m.readUIDMap(state.Pid)
	if err != nil {
		return err
	}
	if strings.Join(strings.Fields(string(mapping)), " ") != "0 0 4294967295" {
		return errors.New("暂不支持启用 userns-remap 的目标容器。")
	}
	source := layout(u).Socket
	before, err := socketIdentity(source)
	if err != nil {
		return err
	}
	if err = m.worker(workerRequest{Action: "attach", User: u, PID: state.Pid, Source: source, Destination: o.SocketPath}, false); err != nil {
		return err
	}
	after, err := socketIdentity(source)
	if err != nil {
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		return errors.New("rootless daemon 在挂载时重建了 socket，请重新执行 add。")
	}
	latest, err := m.inspect(u, o.Host, item.Id)
	if err != nil {
		return err
	}
	if latest.State.Pid != state.Pid || latest.State.StartedAt != state.StartedAt {
		return errors.New("目标容器在挂载时重启了，请重新执行 add。")
	}
	fmt.Fprintf(m.out, "已挂载到 %s:%s（无需重建容器）。\n容器或 rootless dockerd 重启后，请重新执行 add。\n容器内连接：DOCKER_HOST=unix://%s docker info\n", o.Container, o.SocketPath, o.SocketPath)
	return nil
}
func (m *manager) execute(o options) (int, error) {
	if o.Command == "init" {
		return 0, m.initialize(o)
	}
	// cleanup works even if the rootless account/service has been removed.
	if o.Command == "test" {
		return 0, m.testContainer(o.Args)
	}
	u, err := lookupAccount()
	if err != nil {
		return 0, err
	}
	switch o.Command {
	case "add":
		return 0, m.add(u, o)
	case "proxy":
		return 0, m.configureProxy(u, o)
	case "docker":
		return 0, execCommand(dockerArgs(u, "", o.Args...), nil)
	case "logs":
		return 0, execCommand([]string{"tail", "-n", "100", "-F", layout(u).Log + "/dockerd.log"}, nil)
	default:
		r, err := m.systemctl(u, false, o.Command, serviceName)
		if err != nil {
			return 0, err
		}
		fmt.Fprint(m.out, r.Out)
		fmt.Fprint(m.errOut, r.Err)
		if r.Code == 0 && (o.Command == "start" || o.Command == "restart") {
			if err = m.verifyDaemon(u); err != nil {
				return 0, err
			}
			fmt.Fprintln(m.out, "服务已启动；已关联的容器如需更新 socket，请重新执行 add。")
		}
		return r.Code, nil
	}
}
