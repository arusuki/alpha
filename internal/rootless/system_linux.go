package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type account struct {
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
}
type paths struct{ Base, Data, Run, Tmp, Log, Config, Client, Cache, Socket, Unit string }

func layout(u account) paths {
	b := filepath.Join(u.Home, ".docker-rootless")
	return paths{b, b + "/data", b + "/run", b + "/tmp", b + "/log", b + "/config", b + "/client", b + "/cache", b + "/run/docker.sock", filepath.Join(u.Home, ".config/systemd/user", serviceName)}
}
func lookupAccount() (account, error) {
	u, err := user.Lookup(accountName)
	if err != nil {
		return account{}, errors.New("用户 docker-rootless 不存在，请先执行 init。")
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return account{}, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return account{}, err
	}
	a := account{UID: uint32(uid), GID: uint32(gid), Home: filepath.Clean(u.HomeDir)}
	if a.UID == 0 || !filepath.IsAbs(a.Home) || a.Home == "/" {
		return a, errors.New("docker-rootless 必须是具有独立家目录的非 root 用户。")
	}
	resolved, err := filepath.EvalSymlinks(a.Home)
	if err != nil || resolved != a.Home {
		return a, fmt.Errorf("用户家目录必须存在，且不能包含符号链接：%s", a.Home)
	}
	info, err := os.Stat(a.Home)
	if err != nil {
		return a, err
	}
	st := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || st.Uid != a.UID || info.Mode().Perm()&0022 != 0 {
		return a, errors.New("家目录必须属于 docker-rootless，且不能允许组/其他用户写入。")
	}
	for _, c := range a.Home {
		if !strings.ContainsRune("/abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", c) {
			return a, errors.New("家目录路径只能包含字母、数字、/、.、_、-。")
		}
	}
	if len(layout(a).Socket) >= 108 {
		return a, errors.New("家目录路径过长，超出 Unix socket 路径限制。")
	}
	privileged := map[string]bool{"0": true}
	if g, e := user.LookupGroup("docker"); e == nil {
		privileged[g.Gid] = true
	}
	groups, err := u.GroupIds()
	if err != nil {
		return a, err
	}
	groups = append(groups, u.Gid)
	seen := map[uint32]bool{}
	for _, g := range groups {
		if privileged[g] {
			return a, errors.New("docker-rootless 用户属于 root/docker 组，请先移除该权限再使用独立 daemon。")
		}
		id, e := strconv.ParseUint(g, 10, 32)
		if e != nil {
			return a, e
		}
		if !seen[uint32(id)] {
			a.Groups = append(a.Groups, uint32(id))
			seen[uint32(id)] = true
		}
	}
	return a, nil
}
func userEnv(u account) []string {
	return []string{"PATH=" + systemPath, "HOME=" + u.Home, "USER=" + accountName, "LOGNAME=" + accountName, "LANG=C.UTF-8", fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", u.UID), fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", u.UID)}
}
func userCommand(u account, args ...string) []string {
	cmd := []string{"runuser", "-u", accountName, "--", "env", "-i"}
	cmd = append(cmd, userEnv(u)...)
	return append(cmd, args...)
}

var rootDockerEnv = []string{"PATH=" + systemPath, "LANG=C.UTF-8", "HOME=/root"}

type result struct {
	Out, Err string
	Code     int
}
type runner func([]string, []string, time.Duration, bool) (result, error)
type manager struct {
	run         runner
	worker      func(workerRequest, bool) error
	out, errOut io.Writer
	readUIDMap  func(int) ([]byte, error)
}

func newManager() *manager {
	return &manager{run: runCommand, worker: runWorker, out: os.Stdout, errOut: os.Stderr, readUIDMap: func(pid int) ([]byte, error) { return os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid)) }}
}
func runCommand(args, env []string, timeout time.Duration, check bool) (result, error) {
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	// Keep the caller's process group so terminal signals also reach runuser,
	// systemctl and Docker. CommandContext bounds non-interactive operations.
	cmd.WaitDelay = time.Second
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	r := result{Out: out.String(), Err: stderr.String()}
	if ctx.Err() != nil {
		return r, fmt.Errorf("命令超时：%s", args[0])
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return r, err
		}
		r.Code = ee.ExitCode()
		if r.Code < 0 {
			r.Code = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
		}
	}
	if check && r.Code != 0 {
		message := r.Err
		if message == "" {
			message = r.Out
		}
		return r, fmt.Errorf("%s\n%s", shellJoin(args), strings.TrimSpace(message))
	}
	return r, nil
}
func (m *manager) userRun(u account, args []string, timeout time.Duration, check bool) (result, error) {
	return m.run(userCommand(u, args...), nil, timeout, check)
}
func (m *manager) systemctl(u account, check bool, args ...string) (result, error) {
	return m.userRun(u, append([]string{"systemctl", "--user", "--no-pager"}, args...), 150*time.Second, check)
}
func dockerArgs(u account, host string, args ...string) []string {
	if host != "" {
		return append([]string{"docker", "--host", host}, args...)
	}
	p := layout(u)
	return userCommand(u, append([]string{"docker", "--config", p.Client, "--host", "unix://" + p.Socket}, args...)...)
}
func (m *manager) docker(u account, host string, args ...string) (result, error) {
	var env []string
	if host != "" {
		env = rootDockerEnv
	}
	return m.run(dockerArgs(u, host, args...), env, 60*time.Second, true)
}
func execCommand(args, env []string) error {
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	if env == nil {
		env = os.Environ()
	}
	return syscall.Exec(path, args, env)
}
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-", c)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
func shellJoin(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = shellQuote(s)
	}
	return strings.Join(q, " ")
}
func lockHome(u account) (*os.File, error) {
	f, err := os.Open(u.Home)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Run privileged setup in a disposable re-executed process, never by running Go
// code after fork or changing the manager's credentials. Private requests travel
// over stdin so proxy credentials never appear in the worker's command line.
type workerRequest struct {
	Action              string
	User                account
	Options             options
	PID                 int
	Source, Destination string
	DropPrivileges      bool
}

func runWorker(req workerRequest, asUser bool) error {
	// Execute while still privileged. Dropping credentials in SysProcAttr would
	// require the service account to have execute permission on the binary itself,
	// even through an inherited FD (e.g. a root-owned 0700 deployment fails).
	// /proc/self/exe also avoids requiring access through private parent directories.
	req.DropPrivileges = asUser
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.Command("/proc/self/exe", "--internal-worker")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if asUser {
		cmd.Env = userEnv(req.User)
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("无法启动操作子进程：%w", err)
	}
	if err = cmd.Wait(); err != nil {
		return fmt.Errorf("操作失败，详见上方错误：%w", err)
	}
	return nil
}
func workerMain() error {
	var r workerRequest
	if err := json.NewDecoder(os.Stdin).Decode(&r); err != nil {
		return err
	}
	switch r.Action {
	case "prepare", "show-proxy":
		if r.DropPrivileges {
			if err := dropWorkerPrivileges(r.User); err != nil {
				return err
			}
		}
		if os.Geteuid() == 0 || os.Geteuid() != int(r.User.UID) || os.Getegid() != int(r.User.GID) {
			return errors.New("配置操作必须以 docker-rootless 用户身份执行")
		}
		if r.Action == "prepare" {
			return newManager().prepareFiles(r.User, r.Options)
		}
		return showProxy(r.User, os.Stdout)
	case "attach":
		if os.Geteuid() != 0 {
			return errors.New("热挂载需要 root 权限")
		}
		if err := validateContainerPath(r.Destination); err != nil {
			return err
		}
		return attachSocket(r.PID, r.Source, r.Destination, r.User.UID, os.Stdout)
	default:
		return errors.New("未知内部操作")
	}
}

// Only call from the disposable worker, before any configuration access. Use
// syscall's process-wide credential setters: raw unix.Set* calls would only
// change the calling OS thread in a multithreaded Go process.
func dropWorkerPrivileges(u account) error {
	if u.UID == 0 || u.GID == 0 {
		return errors.New("配置操作必须降权到非 root 用户和组")
	}
	groups := make([]int, len(u.Groups))
	for i, gid := range u.Groups {
		if gid == 0 {
			return errors.New("配置操作不允许保留 root 附加组")
		}
		groups[i] = int(gid)
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("设置配置子进程附加组失败：%w", err)
	}
	if err := syscall.Setresgid(int(u.GID), int(u.GID), int(u.GID)); err != nil {
		return fmt.Errorf("设置配置子进程 GID 失败：%w", err)
	}
	if err := syscall.Setresuid(int(u.UID), int(u.UID), int(u.UID)); err != nil {
		return fmt.Errorf("设置配置子进程 UID 失败：%w", err)
	}
	if err := os.Chdir(u.Home); err != nil {
		return fmt.Errorf("进入配置子进程家目录失败：%w", err)
	}
	return nil
}

func subidRange(text, name string, uid uint32, occupied []uint64) (uint64, bool, error) {
	type interval struct{ start, end uint64 }
	var ranges []interval
	enough := false
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		p := strings.Split(line, ":")
		if len(p) != 3 {
			return 0, false, fmt.Errorf("subuid/subgid 中有无效记录：%s", line)
		}
		start, e1 := strconv.ParseUint(strings.TrimSpace(p[1]), 10, 32)
		count, e2 := strconv.ParseUint(strings.TrimSpace(p[2]), 10, 32)
		if e1 != nil || e2 != nil || count == 0 || start+count > 1<<32 {
			return 0, false, fmt.Errorf("subuid/subgid 中有无效记录：%s", line)
		}
		ranges = append(ranges, interval{start, start + count})
		if (p[0] == name || p[0] == strconv.FormatUint(uint64(uid), 10)) && count >= 65536 {
			enough = true
		}
	}
	if enough {
		return 0, false, nil
	}
	for _, id := range occupied {
		ranges = append(ranges, interval{id, id + 1})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	candidate := uint64(100000)
	for _, r := range ranges {
		if candidate+65536 <= r.start {
			break
		}
		if candidate < r.end {
			candidate = r.end
		}
	}
	if candidate+65536 > 1<<32-1 {
		return 0, false, errors.New("没有可用的 subordinate UID/GID 范围。")
	}
	return candidate, true, nil
}
func (m *manager) configureSubids(u account) error {
	for _, s := range []struct{ path, option, db string }{{"/etc/subuid", "--add-subuids", "passwd"}, {"/etc/subgid", "--add-subgids", "group"}} {
		// getent enumerates NSS accounts as pwd.getpwall/grp.getgrall did in Python.
		r, err := m.run([]string{"getent", s.db}, nil, 60*time.Second, true)
		if err != nil {
			return err
		}
		var ids []uint64
		for _, line := range strings.Split(strings.TrimSpace(r.Out), "\n") {
			p := strings.Split(line, ":")
			if len(p) < 3 {
				return errors.New("无法解析系统账户列表")
			}
			id, err := strconv.ParseUint(p[2], 10, 32)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		data, err := os.ReadFile(s.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		start, needed, err := subidRange(string(data), accountName, u.UID, ids)
		if err != nil {
			return err
		}
		if needed {
			if _, err = m.run([]string{"usermod", s.option, fmt.Sprintf("%d-%d", start, start+65535), accountName}, nil, 60*time.Second, true); err != nil {
				return err
			}
		}
	}
	return nil
}
