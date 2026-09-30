package bastion

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"project-alpha/internal/sshkeys"
)

type Account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Home     string `json:"home"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Enabled  bool   `json:"enabled"`
}

func lookupAccount(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, fmt.Errorf("本机账号不存在")
	}
	uid, e := strconv.Atoi(u.Uid)
	gid, e2 := strconv.Atoi(u.Gid)
	if e != nil || e2 != nil || uid == 0 || !filepath.IsAbs(u.HomeDir) || filepath.Clean(u.HomeDir) == "/" {
		return Account{}, fmt.Errorf("跳板账号须为有独立 home 的非 root 本机用户")
	}
	return Account{Username: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

// editKey operates beneath a pinned home directory, rejects symlinked key files,
// serializes with flock, and atomically replaces only the managed member lines.
func editKey(a Account, member, key string) error {
	current, err := lookupAccount(a.Username)
	if err != nil {
		return err
	}
	if current.Home != a.Home || current.UID != a.UID || current.GID != a.GID {
		return fmt.Errorf("跳板账号身份或 home 已改变，拒绝修改")
	}
	return editHome(a, member, key)
}
func editHome(a Account, member, key string) error {
	canonical, err := filepath.EvalSymlinks(a.Home)
	if err != nil {
		return err
	}
	if canonical != filepath.Clean(a.Home) {
		return fmt.Errorf("跳板 home 不能经过符号链接")
	}
	home, err := os.OpenRoot(a.Home)
	if err != nil {
		return err
	}
	defer home.Close()
	pinned, e := home.Open(".")
	if e != nil {
		return e
	}
	homeInfo, e := pinned.Stat()
	pinned.Close()
	if e != nil {
		return e
	}
	stat, ok := homeInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != a.UID || homeInfo.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("跳板 home 必须属于所选账号，且不能允许其他用户写入")
	}
	if err = home.Mkdir(".ssh", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := home.Lstat(".ssh")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(".ssh 必须是普通目录")
	}
	dir, err := home.OpenRoot(".ssh")
	if err != nil {
		return err
	}
	defer dir.Close()
	if os.Geteuid() == 0 {
		if err = home.Chown(".ssh", a.UID, a.GID); err != nil {
			return err
		}
	}
	if err = home.Chmod(".ssh", 0700); err != nil {
		return err
	}
	lock, err := dir.OpenFile(".project-alpha.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("跳板公钥文件正在修改，请重试")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var old []byte
	f, err := dir.OpenFile("authorized_keys", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			f.Close()
			return fmt.Errorf("authorized_keys 必须为普通文件")
		}
		old, err = io.ReadAll(io.LimitReader(f, 1<<20+1))
		f.Close()
		if err != nil {
			return err
		}
		if len(old) > 1<<20 {
			return fmt.Errorf("authorized_keys 超过 1 MiB")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	next, err := sshkeys.Rewrite(old, member, key)
	if err != nil {
		return err
	}
	if key != "" {
		normalized, e := sshkeys.Normalize(key)
		if e != nil {
			return e
		}
		line := normalized + " " + sshkeys.Marker(member)
		next = []byte(strings.Replace(string(next), line, `restrict,port-forwarding,command="/bin/false" `+line, 1))
	}
	temp := ".alpha-" + member + ".tmp"
	out, err := dir.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) { // A stale private temporary file never replaces the live file.
		if err = dir.Remove(temp); err != nil {
			return err
		}
		out, err = dir.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if err != nil {
		return err
	}
	defer dir.Remove(temp)
	if os.Geteuid() == 0 {
		err = out.Chown(a.UID, a.GID)
	}
	if err == nil {
		_, err = out.Write(next)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = dir.Rename(temp, "authorized_keys"); err != nil {
		return err
	}
	df, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	return df.Sync()
}
func validHost(host string) bool {
	return host != "" && len(host) <= 253 && !strings.ContainsAny(host, " /\\\r\n\t\x00@;'\"`$") && !strings.HasPrefix(host, "-")
}
