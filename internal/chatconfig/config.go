// Package chatconfig defines the credential-free configuration shipped with the CLI.
package chatconfig

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const DefaultCommandPath = "/usr/local/bin/alpha-chat"

var username = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)
var commandPath = regexp.MustCompile(`^(/[A-Za-z0-9_.-]+)+$`)

type Defaults struct {
	Server   string `json:"server"`
	Username string `json:"username"`
}

type Settings struct {
	Server      string `json:"server"`
	CommandPath string `json:"command_path"`
}

func Server(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("地址需为不含账号、路径、查询参数的 HTTP(S) 地址")
	}
	u.Path = ""
	return u.String(), nil
}
func (d Defaults) Validate() error {
	normalized, err := Server(d.Server)
	if err != nil || normalized != d.Server {
		return fmt.Errorf("CLI 总控地址无效")
	}
	if !username.MatchString(d.Username) {
		return fmt.Errorf("CLI 使用者标识无效")
	}
	return nil
}
func (s Settings) Validate() error {
	if s.Server != "" {
		if normalized, err := Server(s.Server); err != nil || normalized != s.Server {
			return fmt.Errorf("CLI 总控地址无效")
		}
	}
	if len(s.CommandPath) > 240 || !commandPath.MatchString(s.CommandPath) || path.Clean(s.CommandPath) != s.CommandPath || path.Dir(s.CommandPath) == "/" || strings.HasPrefix(path.Base(s.CommandPath), ".") {
		return fmt.Errorf("CLI 命令路径需为规范的绝对文件路径，名称只接受字母、数字、点、下划线或短横线")
	}
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run"} {
		if s.CommandPath == prefix || strings.HasPrefix(s.CommandPath, prefix+"/") {
			return fmt.Errorf("CLI 不能安装到运行时或虚拟文件系统")
		}
	}
	return nil
}
