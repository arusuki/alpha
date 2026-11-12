package linkhost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

const sudoers = "/etc/sudoers.d/project-alpha-interconnect"
const dropin = "/etc/systemd/system/docker.service.d/project-alpha-interconnect.conf"
const bootGuard = "[Service]\nExecStartPre=" + link.Helper + " guard\n"

func sudoRule(account string) string {
	return "# project-alpha interconnect\n" + account + " ALL=(root) NOPASSWD: " + link.Helper + " \"\"\n"
}
func secureDir(path string) error {
	if path != "/" {
		if err := secureDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err = os.Mkdir(path, 0755); err != nil {
			return err
		}
		st, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || sys.Uid != 0 || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("目录必须由 root 拥有且普通用户不可写: %s", path)
	}
	return nil
}
func Init(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(out, "project-alpha interconnect init [--user USER] [--data-dir DIR] [--uninstall]")
		return nil
	}
	if args[0] != "init" {
		return fmt.Errorf("需要 interconnect init")
	}
	fs := flag.NewFlagSet("interconnect init", flag.ContinueOnError)
	fs.SetOutput(out)
	account := fs.String("user", "project-alpha", "运行 worker 的普通用户")
	dir := fs.String("data-dir", "/var/lib/project-alpha-worker", "已初始化的 worker 数据目录")
	remove := fs.Bool("uninstall", false, "撤销已完成互联清理的节点安装")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多余的参数")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return fmt.Errorf("请管理员在 Linux worker 上运行此初始化命令")
	}
	if *remove {
		return uninstall(ctx)
	}
	if !regexp.MustCompile("^[a-z_][a-z0-9_-]{0,31}$").MatchString(*account) {
		return fmt.Errorf("服务用户名无效")
	}
	u, err := user.Lookup(*account)
	if err != nil || u.Uid == "0" {
		return fmt.Errorf("需要已存在的普通服务用户")
	}
	directory, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	db, err := platform.OpenExistingDatabase(directory, true)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	id, err := db.CheckMode("worker")
	if err != nil {
		return err
	}
	path, err := configuredSocket(db)
	if err != nil {
		return err
	}
	h := host{ctx: ctx, install: installation{Socket: path}}
	daemon, err := h.info()
	if err != nil {
		return err
	}
	install := installation{Worker: id, Daemon: daemon, Directory: directory, User: *account, Socket: path}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src := filepath.Join(filepath.Dir(self), "alpha-interconnect-helper")
	helper, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取同一发布包中的 helper: %w", err)
	}
	for _, d := range []string{stateDir, filepath.Dir(link.Helper), filepath.Dir(sudoers), filepath.Dir(dropin)} {
		if err = secureDir(d); err != nil {
			return err
		}
	}
	lock, err := lockState()
	if err != nil {
		return err
	}
	defer lock.Close()
	var old installation
	err = read(stateDir+"/install.json", &old)
	fresh := os.IsNotExist(err)
	if !fresh && (err != nil || old.Worker != id || old.Daemon != daemon || old.User != *account || old.Directory != directory || old.Socket != path) {
		return fmt.Errorf("已有安装记录不匹配，请先完成原节点清理")
	}
	for path, expected := range map[string]string{sudoers: sudoRule(*account), dropin: bootGuard, link.Helper: ""} {
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if path == link.Helper {
			install.PreviousDigest = fmt.Sprintf("%x", sha256.Sum256(b))
		}
		if e != nil || fresh || expected != "" && string(b) != expected || path == link.Helper && install.PreviousDigest != old.Digest && install.PreviousDigest != old.PreviousDigest {
			return fmt.Errorf("安装路径已有其他内容: %s", path)
		}
	}
	if _, err = h.run(nil, sudoRule(*account), "visudo", "-c", "-f", "-"); err != nil {
		return err
	}
	install.Digest = fmt.Sprintf("%x", sha256.Sum256(helper))
	if fresh {
		var initial ledger
		err := read(stateDir+"/ledger.json", &initial)
		switch {
		case os.IsNotExist(err):
			h.state.Format = ledgerFormat
			if err = h.save(); err != nil {
				return err
			}
		case err != nil || !reflect.DeepEqual(initial, ledger{Format: ledgerFormat}):
			return fmt.Errorf("已有互联归属记录，请核实原安装，保留已有配置")
		}
	} else {
		var current ledger
		if err := read(stateDir+"/ledger.json", &current); err != nil || current.Format != ledgerFormat {
			return fmt.Errorf("原互联归属记录缺失或格式不符，保留已有配置")
		}
		if old.Digest != install.Digest || install.PreviousDigest != install.Digest {
			// Invalidate the old policy before replacing either installation
			// record or binary. Keep live grants until the next check/retry
			// reapplies them with the new helper's policy compiler. Comparing
			// the actual binary also covers an interrupted installation retry.
			h.state = current
			h.state.FirewallHash = ""
			if err = h.save(); err != nil {
				return err
			}
		}
	}
	if err = writeInstall(install); err != nil {
		return err
	}
	if err = atomicFile(link.Helper, helper, 0755); err != nil {
		return err
	}
	if err = atomicFile(dropin, []byte(bootGuard), 0644); err != nil {
		return err
	}
	if _, err = h.run(nil, "", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err = atomicFile(sudoers, []byte(sudoRule(*account)), 0440); err != nil {
		return err
	}
	install.PreviousDigest = ""
	return writeInstall(install)
}
func writeInstall(v installation) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicFile(stateDir+"/install.json", b, 0600)
}
func uninstall(ctx context.Context) error {
	h, lock, err := load(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if len(h.state.Endpoints) != 0 || len(h.state.Routes) != 0 {
		return fmt.Errorf("请在总控退出所有端点并等待清理确认后卸载")
	}
	db, err := platform.OpenExistingDatabase(h.install.Directory, true)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	var pending bool
	if err = db.SQL.QueryRow("SELECT desired<>applied OR EXISTS(SELECT 1 FROM link_claims) FROM link_local").Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("worker 仍有互联配置待清理")
	}
	for path, expected := range map[string]string{sudoers: sudoRule(h.install.User), dropin: bootGuard} {
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || string(b) != expected {
			return fmt.Errorf("安装文件归属不符: %s", path)
		}
	}
	binary, e := os.ReadFile(link.Helper)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if e == nil {
		actual := fmt.Sprintf("%x", sha256.Sum256(binary))
		if actual != h.install.Digest && actual != h.install.PreviousDigest {
			return fmt.Errorf("已安装的 helper 内容改变，请核实归属")
		}
	}
	for _, ch := range chains {
		if err = h.chain(ch, nil, true); err != nil {
			return err
		}
	}
	for _, path := range []string{sudoers, dropin, link.Helper} {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err = h.run(nil, "", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for _, name := range strings.Fields("install.json ledger.json lock") {
		if err = os.Remove(filepath.Join(stateDir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(stateDir)
}
