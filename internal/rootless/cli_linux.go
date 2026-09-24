// Package rootless manages an independent rootless Docker daemon on Linux.
package rootless

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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
	Command       string
	Proxies       map[string]string
	Clear         bool
	AllowLoopback *bool
	Container     string
	Host          string
	SocketPath    string
	Args          []string
	Help          bool
}

const usage = `独立 rootless Docker 管理工具（宿主机 sudo 运行）
用法：rootless-docker <命令> [参数]

  init       创建 docker-rootless 用户，配置并启动用户级服务
  proxy      查看/设置 registry 代理；修改后重启服务
  add        热挂载 socket 到已有运行容器
  start      启动独立 rootless 服务
  stop       停止独立 rootless 服务
  restart    重启独立 rootless 服务
  status     查看独立 rootless 服务
  logs       持续显示 daemon 日志（最后 100 行）
  docker     原样传递后续参数给独立 daemon 的 Docker CLI
  test       管理交互测试容器：up|exec|check|cleanup [容器名]

init / proxy：
  --http-proxy URL       HTTP 代理；空字符串清除该项
  --https-proxy URL      HTTPS 代理，通常也是 http:// 地址
  --no-proxy DOMAINS     不走代理的域名/IP，逗号分隔
  --allow-host-loopback  允许经 10.0.2.2 访问宿主机回环服务（不限于代理端口）
  --disable-host-loopback 恢复禁止访问宿主机回环服务
proxy：
  --clear               清除全部代理并重启；保留网络设置
add <容器名或ID>：
  --host URI            本机 rootful Docker（默认 unix:///var/run/docker.sock）
  --socket-path PATH    容器内路径（默认 /var/run/docker.sock）

test：默认容器名 rootless-cli-test，默认镜像 docker:28-cli。
  ROOTLESS_TEST_IMAGE 可指定包含 Docker CLI、sh、tail 的镜像。
  up 创建/启动并挂载；exec 进入 sh；check 运行 hello-world；cleanup 删除测试容器。
`

func parseOptions(args []string) (options, error) {
	o := options{Proxies: map[string]string{}, Host: "unix:///var/run/docker.sock", SocketPath: "/var/run/docker.sock"}
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
	case "init", "proxy", "add", "start", "stop", "restart", "status", "logs", "test":
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
			case "--allow-host-loopback", "--disable-host-loopback":
				if (o.Command != "init" && o.Command != "proxy") || hasValue {
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
				if (proxy && o.Command != "init" && o.Command != "proxy") || (!proxy && o.Command != "add") {
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
	case "add":
		if len(o.Args) != 1 {
			return o, errors.New("add 需要一个容器名或 ID")
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

// Main returns the command's exit code; exec-based commands preserve the CLI's
// terminal, streaming I/O, signals and exit status.
func Main(args []string) int {
	if len(args) == 1 && args[0] == "--internal-worker" {
		if err := workerMain(); err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			return 1
		}
		return 0
	}
	o, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 2
	}
	if o.Help {
		fmt.Print(usage)
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "错误：请在宿主机使用 sudo 运行本工具。")
		return 1
	}
	if err = os.Setenv("PATH", systemPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	m := newManager()
	code, err := m.execute(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	return code
}
