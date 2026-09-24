package rootless

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func daemonConfig(u account) map[string]any {
	p := layout(u)
	return map[string]any{"data-root": p.Data, "exec-root": p.Run + "/exec", "pidfile": p.Run + "/docker.pid", "hosts": []any{"unix://" + p.Socket}, "rootless": true, "group": "0", "log-driver": "local"}
}
func readObject(path string, missingOK bool) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && missingOK {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err = json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s 必须是有效 JSON 对象", filepath.Base(path))
	}
	return obj, nil
}
func networkSettings(u account) (bool, error) {
	obj, err := readObject(layout(u).Config+"/rootlesskit.json", true)
	if err != nil {
		return false, err
	}
	v, ok := obj["allow-host-loopback"]
	if !ok {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("rootlesskit.json 的 allow-host-loopback 必须是布尔值。")
	}
	return b, nil
}
func showProxy(u account, out io.Writer) error {
	obj, err := readObject(layout(u).Config+"/daemon.json", false)
	if err != nil {
		return err
	}
	proxies := map[string]any{}
	if v, ok := obj["proxies"]; ok {
		var valid bool
		proxies, valid = v.(map[string]any)
		if !valid {
			return errors.New("daemon.json 的 proxies 必须是 JSON 对象。")
		}
	}
	display := map[string]any{}
	for _, key := range proxyKeys {
		value := ""
		if v, ok := proxies[key]; ok {
			var valid bool
			value, valid = v.(string)
			if !valid {
				return fmt.Errorf("daemon.json 的 %s 必须是字符串", key)
			}
		}
		if key != "no-proxy" && value != "" {
			u, e := url.Parse(value)
			if e != nil {
				return errors.New("已保存的代理 URL 无效")
			}
			if u.User != nil {
				u.User = nil
				value = u.Scheme + "://***@" + u.Host + u.RequestURI()
				if u.Path == "" && u.RawQuery == "" {
					value = strings.TrimSuffix(value, "/")
				}
			}
		}
		display[key] = value
	}
	allow, err := networkSettings(u)
	if err != nil {
		return err
	}
	display["allow-host-loopback"] = allow
	fmt.Fprintln(out, "已保存的 daemon 代理配置（URL 中的认证信息已隐藏）：")
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(display)
}
func launchText(u account, allow bool, wrapper string) string {
	p := layout(u)
	disable := "true"
	if allow {
		disable = "false"
	}
	env := [][2]string{{"HOME", u.Home}, {"PATH", systemPath}, {"DBUS_SESSION_BUS_ADDRESS", fmt.Sprintf("unix:path=/run/user/%d/bus", u.UID)}, {"XDG_RUNTIME_DIR", p.Run}, {"XDG_CONFIG_HOME", p.Config}, {"XDG_DATA_HOME", p.Base + "/share"}, {"XDG_CACHE_HOME", p.Cache}, {"DOCKER_CONFIG", p.Client}, {"TMPDIR", p.Tmp}, {"DOCKER_TMPDIR", p.Tmp}, {"DOCKERD_ROOTLESS_ROOTLESSKIT_STATE_DIR", p.Run + "/rootlesskit"}, {"DOCKERD_ROOTLESS_ROOTLESSKIT_NET", "slirp4netns"}, {"DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK", disable}}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n" + managed + "set -eu\numask 077\n# Proxy settings are managed solely through daemon.json.\nunset HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy ALL_PROXY all_proxy\n")
	for _, e := range env {
		fmt.Fprintf(&b, "export %s=%s\n", e[0], shellQuote(e[1]))
	}
	b.WriteString("# Keep a lock across exec; never remove another live daemon's socket.\nexec 9>" + shellQuote(p.Base+"/daemon.lock") + "\nflock -n 9 || { echo 'rootless daemon is already running' >&2; exit 1; }\n# Recreate ephemeral state, including files owned by mapped UIDs.\n# systemd has stopped the preceding process group before this runs.\nrootlesskit rm -rf -- " + shellQuote(p.Run) + "\nmkdir -m 700 -- " + shellQuote(p.Run) + "\n\nexec " + shellQuote(wrapper) + " --config-file " + shellQuote(p.Config+"/daemon.json") + "\n")
	return b.String()
}
func unitText(u account) string {
	p := layout(u)
	return managed + fmt.Sprintf(`[Unit]
Description=Independent rootless Docker (docker-rootless)
Requires=dbus.socket
After=dbus.socket
StartLimitIntervalSec=60
StartLimitBurst=3

[Service]
Type=notify
NotifyAccess=all
ExecStart=%s/launch.sh
WorkingDirectory=%s
Restart=always
RestartSec=3
TimeoutStartSec=120
TimeoutStopSec=90
KillMode=mixed
Delegate=yes
LimitNOFILE=infinity
LimitNPROC=infinity
TasksMax=infinity
UMask=0077
StandardOutput=append:%s/dockerd.log
StandardError=append:%s/dockerd.log

[Install]
WantedBy=default.target
`, p.Base, u.Home, p.Log, p.Log)
}

// O_EXCL protects temporary files from symlinks; rename replaces a final symlink
// rather than writing through it. All callers run as the unprivileged account.
func atomicWrite(path string, data []byte, mode os.FileMode, validate func(string) error) error {
	temp := path + ".tmp"
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if validate != nil {
		if err = validate(temp); err != nil {
			return err
		}
	}
	return os.Rename(temp, path)
}
func writeManaged(path, content string, mode os.FileMode) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !strings.HasPrefix(string(data), managed) && !strings.HasPrefix(string(data), "#!/bin/sh\n"+managed) {
		return fmt.Errorf("拒绝覆盖非本工具管理的文件：%s", path)
	}
	return atomicWrite(path, []byte(content), mode, nil)
}
func jsonBytes(obj any) ([]byte, error) {
	data, err := json.MarshalIndent(obj, "", "  ")
	return append(data, '\n'), err
}
func ensurePrivateDirectory(path string) error {
	// Check existing ancestors before MkdirAll, including broken symlinks.
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("管理目录不能是符号链接：%s", path)
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}
func (m *manager) prepareFiles(u account, o options) error {
	p := layout(u)
	for _, path := range []string{p.Base, p.Data, p.Run, p.Tmp, p.Log, p.Config, p.Client, p.Cache, filepath.Dir(p.Unit)} {
		if err := ensurePrivateDirectory(path); err != nil {
			return err
		}
	}
	allow, err := networkSettings(u)
	if err != nil {
		return err
	}
	if o.AllowLoopback != nil {
		allow = *o.AllowLoopback
	}
	config, err := readObject(p.Config+"/daemon.json", true)
	if err != nil {
		return err
	}
	if _, ok := config["containerd"]; ok {
		return errors.New("此独立 daemon 不允许设置外部 containerd。")
	}
	for key, value := range daemonConfig(u) {
		if v, ok := config[key]; ok {
			if key != "log-driver" && !reflect.DeepEqual(v, value) {
				return fmt.Errorf("daemon.json 的隔离配置被修改：%s", key)
			}
		} else {
			config[key] = value
		}
	}
	if o.Clear {
		delete(config, "proxies")
	} else if len(o.Proxies) > 0 {
		proxies := map[string]any{}
		if v, ok := config["proxies"]; ok {
			var valid bool
			proxies, valid = v.(map[string]any)
			if !valid {
				return errors.New("daemon.json 的 proxies 必须是 JSON 对象。")
			}
		}
		for k, v := range o.Proxies {
			proxies[k] = v
		}
		config["proxies"] = proxies
	}
	data, err := jsonBytes(config)
	if err != nil {
		return err
	}
	var validate func(string) error
	if len(o.Proxies) > 0 || o.Clear {
		validate = func(path string) error {
			r, e := m.run([]string{"dockerd", "--validate", "--config-file", path}, nil, 60*time.Second, false)
			if e != nil || r.Code != 0 {
				return errors.New("代理配置未通过 dockerd 校验，原 daemon.json 已保留；代理配置需要 Docker Engine 23.0+。")
			}
			return nil
		}
	}
	if err = atomicWrite(p.Config+"/daemon.json", data, 0600, validate); err != nil {
		return err
	}
	data, err = jsonBytes(map[string]bool{"allow-host-loopback": allow})
	if err != nil {
		return err
	}
	if err = atomicWrite(p.Config+"/rootlesskit.json", data, 0600, nil); err != nil {
		return err
	}
	wrapper, err := exec.LookPath("dockerd-rootless.sh")
	if err != nil {
		return err
	}
	if err = writeManaged(p.Base+"/launch.sh", launchText(u, allow, wrapper), 0700); err != nil {
		return err
	}
	return writeManaged(p.Unit, unitText(u), 0600)
}
