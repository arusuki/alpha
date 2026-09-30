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
const installationDirectory = "/var/lib/project-alpha-jump"
const readerExecutable = "/usr/local/libexec/project-alpha-jump"
const keyFormat = 1
const maxKeyFile = 4 << 20
const keyOptions = `restrict,port-forwarding,command="/bin/false" `

type installation struct {
	Version        int    `json:"version"`
	Ready          bool   `json:"ready"`
	Released       bool   `json:"-"`
	AccountRemoved bool   `json:"-"`
	ControlID      string `json:"control_id"`
	ServiceUser    string `json:"service_user"`
	ServiceUID     int    `json:"service_uid"`
	JumpUID        int    `json:"jump_uid"`
	JumpGID        int    `json:"jump_gid"`
}

type keySnapshot struct {
	Version   int               `json:"version"`
	ControlID string            `json:"control_id"`
	Keys      map[string]string `json:"keys"`
}

type keyStore struct {
	path   string
	owner  int
	lookup func(string) (*user.User, error)
}

func systemKeyStore() keyStore {
	return keyStore{path: installationDirectory, owner: 0, lookup: user.Lookup}
}

func strictJSON(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// Only the installed service identity may publish keys. Neither sshd nor the
// reader opens the control database or obtains permission to modify user homes.
func (s keyStore) open() (*os.Root, installation, error) {
	return s.openInstallation(true)
}

// An explicit privileged adoption may replace a departed service identity.
// The jump UID/GID, file ownership and persisted formats still must match.
func (s keyStore) openForAdoption() (*os.Root, installation, error) {
	return s.openInstallation(false)
}

func (s keyStore) openInstallation(checkService bool) (*os.Root, installation, error) {
	var c installation
	fail := func(err error) (*os.Root, installation, error) {
		return nil, c, fmt.Errorf("alpha-jump 未初始化或安装无效，请在跳板机管理中初始化: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(s.path)
	if err != nil {
		return fail(err)
	}
	if canonical != filepath.Clean(s.path) {
		return fail(fmt.Errorf("安装目录不能经过符号链接"))
	}
	r, err := os.OpenRoot(s.path)
	if err != nil {
		return fail(err)
	}
	check := func() error {
		info, err := r.Stat(".")
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != s.owner || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("安装目录属主或权限无效")
		}
		raw, err := readPrivateFile(r, "installation.json", s.owner, -1, 0022, 16384)
		if err != nil {
			return err
		}
		if err = strictJSON(raw, &c); err != nil {
			return err
		}
		if c.Version != keyFormat || !sshkeys.ID.MatchString(c.ControlID) || c.ServiceUser == "" || c.ServiceUser == JumpUser || c.ServiceUID <= 0 || c.JumpUID <= 0 || c.JumpGID <= 0 || c.ServiceUID == c.JumpUID {
			return fmt.Errorf("安装格式或账号身份无效")
		}
		// Lifecycle markers belong to management, not the reader's installation
		// or key formats. Releasing does not rewrite either authorization file.
		for name, flag := range map[string]*bool{"released": &c.Released, "account-removed": &c.AccountRemoved} {
			raw, err := readPrivateFile(r, name, s.owner, -1, 0022, 64)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if string(raw) != name+"\n" {
				return fmt.Errorf("账号管理状态 %s 无效", name)
			}
			*flag = true
		}
		for name, uid := range map[string]int{c.ServiceUser: c.ServiceUID, JumpUser: c.JumpUID} {
			if name == c.ServiceUser && !checkService {
				continue
			}
			u, err := s.lookup(name)
			if name == JumpUser && c.AccountRemoved {
				if _, absent := err.(user.UnknownUserError); absent {
					continue
				}
			}
			if err != nil || u.Uid != strconv.Itoa(uid) {
				return fmt.Errorf("%s 账号身份已改变", name)
			}
			if name == JumpUser && u.Gid != strconv.Itoa(c.JumpGID) {
				return fmt.Errorf("alpha-jump 组身份已改变")
			}
		}
		return nil
	}
	if err = check(); err != nil {
		r.Close()
		return fail(err)
	}
	return r, c, nil
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

func openKeys(r *os.Root, c installation) (*os.Root, error) {
	info, err := r.Lstat("keys")
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSetgid == 0 || info.Mode().Perm() != 0750 || int(st.Uid) != c.ServiceUID || int(st.Gid) != c.JumpGID {
		return nil, fmt.Errorf("alpha-jump 公钥目录属主或权限无效，请重新检查安装")
	}
	return r.OpenRoot("keys")
}

func loadSnapshot(r *os.Root, c installation) (keySnapshot, error) {
	var v keySnapshot
	raw, err := readPrivateFile(r, "keys.json", c.ServiceUID, c.JumpGID, 0037, maxKeyFile)
	if os.IsNotExist(err) {
		return keySnapshot{Version: keyFormat, ControlID: c.ControlID, Keys: map[string]string{}}, nil
	}
	if err != nil {
		return v, err
	}
	if err = strictJSON(raw, &v); err != nil {
		return v, err
	}
	if v.Version != keyFormat || v.ControlID != c.ControlID || v.Keys == nil {
		return v, fmt.Errorf("公钥清单格式或 control 实例不匹配，拒绝授权")
	}
	for id, key := range v.Keys {
		if !sshkeys.ID.MatchString(id) {
			return v, fmt.Errorf("公钥池条目标识无效")
		}
		normalized, err := sshkeys.Normalize(key)
		if err != nil || normalized != key {
			return v, fmt.Errorf("公钥清单包含无效公钥")
		}
	}
	return v, nil
}

func checkWriter(c installation, id string) error {
	if c.AccountRemoved {
		return fmt.Errorf("alpha-jump 账号已删除，请添加账号后再管理公钥")
	}
	if c.Released {
		return fmt.Errorf("alpha-jump 已取消接管；账号、工具、data 和现有授权保留，请重新接管后管理公钥")
	}
	if !c.Ready {
		return fmt.Errorf("alpha-jump 初始化尚未完成，请在跳板机管理中重新安装")
	}
	if c.ControlID != id {
		return fmt.Errorf("alpha-jump 由其他 control 管理，请先接管已有账号和公钥池")
	}
	if os.Geteuid() != c.ServiceUID {
		return fmt.Errorf("请使用初始化时指定的普通用户 %s 运行 control", c.ServiceUser)
	}
	return nil
}

func (s keyStore) checkWriter(id string) error {
	r, c, err := s.open()
	if err != nil {
		return err
	}
	defer r.Close()
	if err = checkWriter(c, id); err != nil {
		return err
	}
	k, err := openKeys(r, c)
	if err != nil {
		return err
	}
	defer k.Close()
	_, err = loadSnapshot(k, c)
	return err
}

func (s keyStore) update(control string, change func(*keySnapshot) error) error {
	r, c, err := s.open()
	if err != nil {
		return err
	}
	defer r.Close()
	if err = checkWriter(c, control); err != nil {
		return err
	}
	k, err := openKeys(r, c)
	if err != nil {
		return err
	}
	defer k.Close()
	lock, err := k.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("公钥清单锁文件无效")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("公钥清单正在更新，请重试")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Recheck after acquiring the publication lock: release/delete may have
	// completed since the initial manifest read.
	latest, current, err := s.open()
	if err != nil {
		return err
	}
	latest.Close()
	if current.ControlID != c.ControlID || current.ServiceUID != c.ServiceUID || current.ServiceUser != c.ServiceUser {
		return fmt.Errorf("alpha-jump 管理归属已变更，请刷新后重试")
	}
	if err = checkWriter(current, control); err != nil {
		return err
	}
	v, err := loadSnapshot(k, c)
	if err != nil {
		return err
	}
	if err = change(&v); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > maxKeyFile {
		return fmt.Errorf("公钥清单已满")
	}
	temp := ".keys-" + platform.RandomHex(16)
	f, err := k.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer k.Remove(temp)
	// The setgid directory supplies alpha-jump's read-only group. Chmod is
	// explicit because the main process deliberately uses umask 0077.
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(0640)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = k.Rename(temp, "keys.json"); err != nil {
		return err
	}
	d, err := k.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s keyStore) authorizedKeys(name, uid string, out io.Writer) error {
	if name != JumpUser {
		return fmt.Errorf("仅允许查询 alpha-jump")
	}
	r, c, err := s.open()
	if err != nil {
		return err
	}
	defer r.Close()
	if !c.Ready || c.AccountRemoved {
		return fmt.Errorf("alpha-jump 初始化尚未完成")
	}
	if uid != strconv.Itoa(c.JumpUID) {
		return fmt.Errorf("alpha-jump 查询身份不匹配")
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
	var b bytes.Buffer
	for _, id := range ids {
		fmt.Fprintln(&b, keyOptions+v.Keys[id]+" project-alpha:pool:"+id)
	}
	_, err = out.Write(b.Bytes())
	return err
}
