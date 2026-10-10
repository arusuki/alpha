// Package rootless manages an independent rootless Docker daemon on Linux.
package rootless

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const accountName = "docker-rootless"
const serviceName = "docker-rootless.service"
const managed = "# Managed by rootless-docker\n"
const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var proxyKeys = []string{"http-proxy", "https-proxy", "no-proxy"}

type options struct {
	Command        string
	Proxies        map[string]string
	Clear          bool
	AllowLoopback  *bool
	Container      string
	Host           string
	SocketPath     string
	ControlSocket  string
	HostNamespaces bool
	SocketGID      int
	Args           []string
	Help           bool
}

const usage = `rootless Docker daemon/client 管理工具
用法：rootless-docker <命令> [参数]

  daemon     启动管理服务及宿主机 rootless dockerd；退出时停止 dockerd
  add        热挂载 socket 到已有运行容器，并保存关联
  remove     卸载本工具的 socket 挂载并删除关联
  list       查看关联和最近的恢复错误
  proxy      查看/设置 registry 代理；修改后重启并恢复挂载
  restart    重启 rootless dockerd 并恢复挂载
  status     查看管理服务和 rootless dockerd 状态
  logs       持续显示 dockerd 日志（最后 100 行）
  docker     使用本地 Docker CLI 连接管理服务转发的 rootless API
  test       管理测试容器：up|exec|check|cleanup [容器名]

客户端通过 ROOTLESS_CONTROL_SOCKET 指定管理 socket，默认：
  /run/rootless-docker/control.sock
管理 socket 为 0660；获授权用户无需 sudo，权限等同宿主机管理员。

daemon：
  --host-namespaces     在 privileged + pid:host 容器中进入宿主机环境
  --socket-gid GID      管理 socket 的宿主机授权组（默认 0）
  --http-proxy URL / --https-proxy URL / --no-proxy DOMAINS
  --allow-host-loopback / --disable-host-loopback
proxy：
  --http-proxy URL      HTTP 代理；空字符串清除该项
  --https-proxy URL     HTTPS registry 代理
  --no-proxy DOMAINS    不走代理的域名/IP，逗号分隔
  --allow-host-loopback 允许 rootless 网络访问宿主机回环服务
  --disable-host-loopback 禁止访问宿主机回环服务
  --clear              清除代理；保留网络设置
add / remove <容器名或ID>：
  --host URI           本机 rootful Docker（默认 unix:///var/run/docker.sock）
  --socket-path PATH   容器内路径（默认 /var/run/docker.sock）

test：默认 rootless-cli-test；exec 还需客户端能访问宿主机 Docker。
管理服务生命周期由前台进程或 Docker Compose start/stop 控制。
`

func parseOptions(args []string) (options, error) {
	o := options{ControlSocket: controlSocket(), Proxies: map[string]string{}, Host: "unix:///var/run/docker.sock", SocketPath: "/var/run/docker.sock"}
	if len(args) == 0 {
		return o, errors.New("需要指定命令；使用 --help 查看用法")
	}
	if args[0] == "--help" || args[0] == "-h" {
		o.Help = true
		return o, nil
	}
	o.Command = args[0]
	switch o.Command {
	case "docker":
		o.Args = args[1:]
		if len(o.Args) > 0 && o.Args[0] == "--" {
			o.Args = o.Args[1:]
		}
		return o, nil
	case "daemon", "proxy", "add", "remove", "list", "restart", "status", "logs", "test":
	default:
		return o, fmt.Errorf("未知命令：%s", o.Command)
	}
	positional := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !positional && (arg == "--help" || arg == "-h") {
			o.Help = true
			return o, nil
		}
		if !positional && arg == "--" {
			positional = true
			continue
		}
		if !positional && strings.HasPrefix(arg, "-") {
			key, value, hasValue := strings.Cut(arg, "=")
			switch key {
			case "--host-namespaces":
				if o.Command != "daemon" || hasValue {
					return o, fmt.Errorf("不支持参数：%s", key)
				}
				o.HostNamespaces = true
			case "--socket-gid":
				if o.Command != "daemon" {
					return o, fmt.Errorf("不支持参数：%s", key)
				}
				if !hasValue {
					i++
					if i == len(args) {
						return o, errors.New("--socket-gid 缺少值")
					}
					value = args[i]
				}
				gid, err := strconv.ParseUint(value, 10, 31)
				if err != nil {
					return o, errors.New("--socket-gid 必须是非负整数")
				}
				o.SocketGID = int(gid)
			case "--allow-host-loopback", "--disable-host-loopback":
				if (o.Command != "daemon" && o.Command != "proxy") || hasValue {
					return o, fmt.Errorf("不支持参数：%s", key)
				}
				b := key == "--allow-host-loopback"
				if o.AllowLoopback != nil && *o.AllowLoopback != b {
					return o, errors.New("--allow-host-loopback 与 --disable-host-loopback 不能同时使用")
				}
				o.AllowLoopback = &b
			case "--clear":
				if o.Command != "proxy" || hasValue {
					return o, fmt.Errorf("不支持参数：%s", key)
				}
				o.Clear = true
			case "--http-proxy", "--https-proxy", "--no-proxy", "--host", "--socket-path":
				proxy := key != "--host" && key != "--socket-path"
				if (proxy && o.Command != "daemon" && o.Command != "proxy") || (!proxy && o.Command != "add" && o.Command != "remove") {
					return o, fmt.Errorf("不支持参数：%s", key)
				}
				if !hasValue {
					i++
					if i == len(args) || strings.HasPrefix(args[i], "--") {
						return o, fmt.Errorf("参数 %s 缺少值", key)
					}
					value = args[i]
				}
				var err error
				switch key {
				case "--http-proxy", "--https-proxy":
					err = validateProxyURL(value)
					o.Proxies[key[2:]] = value
				case "--no-proxy":
					err = validateNoProxy(value)
					o.Proxies[key[2:]] = value
				case "--host":
					err = validateHost(value)
					o.Host = value
				case "--socket-path":
					err = validateContainerPath(value)
					o.SocketPath = value
				}
				if err != nil {
					return o, err
				}
			default:
				return o, fmt.Errorf("未知参数：%s", key)
			}
		} else {
			o.Args = append(o.Args, arg)
		}
	}
	if o.Clear && len(o.Proxies) > 0 {
		return o, errors.New("--clear 不能与 --http-proxy、--https-proxy 或 --no-proxy 同时使用。")
	}
	switch o.Command {
	case "add", "remove":
		if len(o.Args) != 1 {
			return o, errors.New("add/remove 需要一个容器名或 ID")
		}
		o.Container = o.Args[0]
	case "test":
		if len(o.Args) == 0 {
			o.Help = true
			return o, nil
		}
		if len(o.Args) > 2 {
			return o, errors.New("test 参数过多")
		}
		switch o.Args[0] {
		case "help":
			o.Help = true
		case "up", "exec", "check", "cleanup":
		default:
			return o, errors.New("test 需要 up、exec、check 或 cleanup")
		}
		if len(o.Args) == 2 && !validContainerName(o.Args[1]) {
			return o, errors.New("容器名格式不合法")
		}
	default:
		if len(o.Args) > 0 {
			return o, errors.New("命令不接受位置参数")
		}
	}
	return o, nil
}

func validateProxyURL(value string) error {
	if value == "" {
		return nil
	}
	bad := errors.New("代理必须是 http:// 或 https:// 的完整 URL（或空字符串），且无查询参数/片段。")
	for _, c := range value {
		if unicode.IsSpace(c) || c < 32 || c == 127 {
			return bad
		}
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return bad
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return bad
		}
	}
	return nil
}
func validateNoProxy(value string) error {
	for _, c := range value {
		if c < 32 || c == 127 {
			return errors.New("no-proxy 不能包含换行或控制字符。")
		}
	}
	return nil
}
func validateHost(value string) error {
	if !strings.HasPrefix(value, "unix:///") || strings.ContainsRune(value, 0) {
		return errors.New("只支持本机 unix:///绝对路径 socket")
	}
	return nil
}
func validateContainerPath(value string) error {
	if !strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsRune(value, 0) {
		return errors.New("容器 socket 路径必须是无 .. 的绝对文件路径")
	}
	for _, s := range strings.Split(value, "/") {
		if s == ".." {
			return errors.New("容器 socket 路径必须是无 .. 的绝对文件路径")
		}
	}
	return nil
}
func validContainerName(s string) bool {
	for i, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '_' || c == '.' || c == '-') {
			continue
		}
		return false
	}
	return s != ""
}

func controlSocket() string {
	if path := os.Getenv("ROOTLESS_CONTROL_SOCKET"); path != "" {
		return path
	}
	return runtimeDirectory + "/control.sock"
}

// Main keeps interactive Docker execution on the client, never in the daemon.
func Main(args []string) int {
	var err error
	if len(args) == 1 && args[0] == "--internal-worker" {
		err = workerMain()
	} else if len(args) == 3 && args[0] == "--internal-supervisor" {
		err = supervise(args[1], args[2])
	} else {
		var o options
		o, err = parseOptions(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			return 2
		}
		if o.Help {
			fmt.Print(usage)
			return 0
		}
		if !filepath.IsAbs(o.ControlSocket) || len(o.ControlSocket) >= 108 {
			err = errors.New("管理 socket 必须是有效绝对路径，长度小于 108 字节")
		} else if o.Command == "daemon" {
			if os.Geteuid() != 0 {
				err = errors.New("daemon 需要宿主机 root 权限或 privileged 容器")
			} else if o.HostNamespaces {
				err = enterHost(args)
			} else {
				os.Setenv("PATH", systemPath)
				err = runDaemon(o)
			}
		} else {
			err = runClient(o, args)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	return 0
}
