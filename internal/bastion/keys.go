package bastion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

const JumpUser = "alpha-jump"
const WorkerUser = "alpha-worker"
const installationDirectory = "/var/lib/project-alpha-jump"
const readerExecutable = "/usr/local/libexec/project-alpha-jump"
const keyFormat = 2
const maxKeyFile = 4 << 20

type installation struct {
	Version    int    `json:"version"`
	ControlKey string `json:"control_key"`
	ControlURL string `json:"control_url"`
	ListenHost string `json:"listen_host"`
	StatusPort int    `json:"status_port"`
	JumpUID    int    `json:"jump_uid"`
	JumpGID    int    `json:"jump_gid"`
	WorkerUID  int    `json:"worker_uid"`
	WorkerGID  int    `json:"worker_gid"`
}
type keySnapshot struct {
	Version int               `json:"version"`
	Keys    map[string]string `json:"keys"`
}
type keyStore struct {
	path   string
	owner  int
	lookup func(string) (*user.User, error)
}

func systemKeyStore() keyStore { return keyStore{installationDirectory, 0, user.Lookup} }
func strictJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("JSON 包含额外数据")
	}
	return nil
}
func readPrivateFile(r *os.Root, name string, uid, gid int, forbidden os.FileMode, limit int64) ([]byte, error) {
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || int(st.Uid) != uid || gid >= 0 && int(st.Gid) != gid || info.Mode().Perm()&forbidden != 0 {
		return nil, fmt.Errorf("%s 文件类型、属主或权限无效", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s 文件过大", name)
	}
	return raw, err
}
func (s keyStore) open() (*os.Root, installation, error) { return s.openManifest(true) }
func (s keyStore) openManifest(checkAccounts bool) (*os.Root, installation, error) {
	var c installation
	canonical, err := filepath.EvalSymlinks(s.path)
	if err != nil {
		return nil, c, err
	}
	if canonical != filepath.Clean(s.path) {
		return nil, c, fmt.Errorf("安装目录不能经过符号链接")
	}
	r, err := os.OpenRoot(s.path)
	if err != nil {
		return nil, c, err
	}
	check := func() error {
		info, e := r.Stat(".")
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != s.owner || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("安装目录属主或权限无效")
		}
		raw, e := readPrivateFile(r, "installation.json", s.owner, -1, 0022, 16384)
		if e != nil {
			return e
		}
		if e = strictJSON(raw, &c); e != nil {
			return e
		}
		key, e := sshkeys.Normalize(c.ControlKey)
		if e != nil || key != c.ControlKey || c.Version != keyFormat || c.JumpUID <= 0 || c.JumpGID <= 0 || c.WorkerUID <= 0 || c.WorkerGID <= 0 || c.WorkerUID == c.JumpUID || c.StatusPort < 1024 || c.StatusPort > 65535 {
			return fmt.Errorf("跳板安装格式无效或版本不匹配，请使用新数据目录")
		}
		if _, e = platform.InternalIP(c.ListenHost); e != nil {
			return e
		}
		if _, e = proxyTarget(c.ControlURL); e != nil {
			return e
		}
		if checkAccounts {
			for name, ids := range map[string][2]int{JumpUser: {c.JumpUID, c.JumpGID}, WorkerUser: {c.WorkerUID, c.WorkerGID}} {
				u, e := s.lookup(name)
				if e != nil || u.Uid != strconv.Itoa(ids[0]) || u.Gid != strconv.Itoa(ids[1]) {
					return fmt.Errorf("%s 系统身份不匹配", name)
				}
			}
		}
		return nil
	}
	if err = check(); err != nil {
		r.Close()
		return nil, c, err
	}
	return r, c, nil
}
func openKeys(r *os.Root, c installation) (*os.Root, error) {
	info, err := r.Lstat("keys")
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0750 || info.Mode()&os.ModeSetgid == 0 || int(st.Uid) != c.WorkerUID || int(st.Gid) != c.JumpGID {
		return nil, fmt.Errorf("公钥目录属主或权限无效")
	}
	return r.OpenRoot("keys")
}
func validateSnapshot(v keySnapshot, controlKey string) error {
	if v.Version != keyFormat || v.Keys == nil {
		return fmt.Errorf("公钥格式不匹配，请使用新数据目录")
	}
	for id, key := range v.Keys {
		normal, err := sshkeys.Normalize(key)
		if !sshkeys.ID.MatchString(id) || err != nil || normal != key {
			return fmt.Errorf("公钥清单包含无效条目")
		}
		if key == controlKey {
			return fmt.Errorf("管理公钥不能作为成员公钥")
		}
	}
	return nil
}
func loadSnapshot(r *os.Root, c installation) (keySnapshot, error) {
	var v keySnapshot
	raw, err := readPrivateFile(r, "keys.json", c.WorkerUID, c.JumpGID, 0037, maxKeyFile)
	if os.IsNotExist(err) {
		return keySnapshot{Version: keyFormat, Keys: map[string]string{}}, nil
	}
	if err != nil {
		return v, err
	}
	if err = strictJSON(raw, &v); err != nil {
		return v, err
	}
	return v, validateSnapshot(v, c.ControlKey)
}
func updateKeys(r *os.Root, c installation, change func(*keySnapshot) error) (keySnapshot, error) {
	if os.Geteuid() != c.WorkerUID {
		return keySnapshot{}, fmt.Errorf("公钥管理必须以 alpha-worker 运行")
	}
	k, err := openKeys(r, c)
	if err != nil {
		return keySnapshot{}, err
	}
	defer k.Close()
	lock, err := k.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return keySnapshot{}, err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil {
		return keySnapshot{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || int(st.Uid) != c.WorkerUID || int(st.Gid) != c.JumpGID || info.Mode().Perm() != 0600 {
		return keySnapshot{}, fmt.Errorf("公钥锁文件无效")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return keySnapshot{}, fmt.Errorf("公钥正在更新，请重试")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	v, err := loadSnapshot(k, c)
	if err != nil {
		return v, err
	}
	if err = change(&v); err != nil {
		return v, err
	}
	if err = validateSnapshot(v, c.ControlKey); err != nil {
		return v, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if len(raw) > maxKeyFile {
		return v, fmt.Errorf("公钥清单已满")
	}
	name := ".keys-" + platform.RandomHex(16)
	f, err := k.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return v, err
	}
	defer k.Remove(name)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(0640)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return v, err
	}
	if closeErr != nil {
		return v, closeErr
	}
	if err = k.Rename(name, "keys.json"); err != nil {
		return v, err
	}
	dir, err := k.Open(".")
	if err != nil {
		return v, err
	}
	defer dir.Close()
	return v, dir.Sync()
}
func (s keyStore) authorizedKeys(name, uid string, out io.Writer) error {
	r, c, err := s.open()
	if err != nil {
		return err
	}
	defer r.Close()
	if name != JumpUser || uid != strconv.Itoa(c.JumpUID) {
		return fmt.Errorf("SSH 查询身份不匹配")
	}
	k, err := openKeys(r, c)
	if err != nil {
		return err
	}
	defer k.Close()
	v, err := loadSnapshot(k, c)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(v.Keys))
	for id := range v.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err = fmt.Fprintln(out, `restrict,port-forwarding,command="/bin/false" `+v.Keys[id]+" project-alpha:"+id); err != nil {
			return err
		}
	}
	return nil
}
