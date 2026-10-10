package updater

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RecoveryInstructions struct {
	Command     string `json:"command"`
	Note        string `json:"note"`
	LogPath     string `json:"log_path"`
	PendingPath string `json:"pending_path"`
	Required    bool   `json:"required"`
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// Recovery commands contain paths and release selection, never credentials.
func (p ServicePlan) Recovery() RecoveryInstructions {
	args := []string{p.Command, "--role", p.Role, "--data-dir", p.Directory, "--bin-dir", filepath.Dir(p.Executable), "--repo", p.Repo}
	tag := p.Tag
	if p.Prepared != nil {
		tag = p.Prepared.Tag
	}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	if p.Prerelease {
		args = append(args, "--prerelease")
	}
	if p.ServicesOnly {
		args = append(args, "--services-only")
	}
	for i := range args {
		args[i] = shellQuote(args[i])
	}
	r := RecoveryInstructions{Command: strings.Join(args, " "), LogPath: filepath.Join(p.Directory, "update.log"), PendingPath: filepath.Join(filepath.Dir(p.Executable), ".alpha-update-pending"), Note: "在此目标节点使用原服务账号操作：先修复报告的问题并停止使用该数据目录的主程序，再执行命令；成功后按原配置启动主程序。updater 不调用 sudo，账号须有数据、安装目录及已部署服务的 Docker 访问权限。若提示需要人工恢复，先按日志和恢复记录核对数据库、二进制及容器，勿直接删除标记重试。"}
	if p.Proxy != "" {
		r.Note += " 请先在该账号环境中设置原下载代理 HTTPS_PROXY；代理凭据不会写入此命令。"
	}
	r.Note += " 如需 GitHub 认证，请通过 GH_TOKEN 环境变量提供，勿把 Token 写入命令参数。"
	r.RefreshPending()
	return r
}

func (r *RecoveryInstructions) RefreshPending() {
	_, err := os.Stat(r.PendingPath)
	r.Required = !os.IsNotExist(err)
}

func (r ServiceResult) Valid() bool {
	return !r.Finished.IsZero() && ((r.State == "completed" && r.Error == "" && r.Recovery == nil) || (r.State == "failed" && r.Error != ""))
}

func (p ServicePlan) Result(err error) ServiceResult {
	r := ServiceResult{State: "completed", Finished: time.Now().UTC()}
	if err == nil {
		return r
	}
	r.State = "failed"
	r.Error = err.Error()
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(key); token != "" {
			r.Error = strings.ReplaceAll(r.Error, token, "[redacted]")
		}
	}
	if proxy, e := url.Parse(p.Proxy); e == nil && proxy.User != nil {
		userinfo := proxy.User.String()
		proxy.User = nil
		r.Error = strings.ReplaceAll(r.Error, p.Proxy, proxy.String())
		r.Error = strings.ReplaceAll(r.Error, userinfo, "[redacted]")
	}
	recovery := p.Recovery()
	r.Recovery = &recovery
	return r
}

// CLI and automatic installation publish the same authoritative outcome. Read-only
// checks and database-only maintenance do not erase an unresolved update failure.
func reportCLI(o options, out io.Writer, update func(io.Writer) error) error {
	if o.check || o.databaseOnly || o.role == "share-node" {
		return update(out)
	}
	command, err := os.Executable()
	if err != nil {
		return err
	}
	p := ServicePlan{Command: command, Directory: o.directory, Executable: filepath.Join(o.binDir, "project-alpha"), Role: o.role, Repo: o.repo, Tag: o.tag, Prerelease: o.prerelease, Proxy: o.proxy, ServicesOnly: o.servicesOnly}
	log, err := os.OpenFile(filepath.Join(o.directory, "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		defer log.Close()
		err = update(io.MultiWriter(out, log))
	}
	result := p.Result(err)
	if err != nil && log != nil {
		fmt.Fprintln(log, result.Error)
	}
	if e := WriteJSON(filepath.Join(o.directory, "update-result.json"), result); e != nil {
		return errors.Join(err, fmt.Errorf("保存更新结果失败：%w", e))
	}
	return err
}
