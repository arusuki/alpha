package bastion

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"project-alpha/internal/sshkeys"
)

type controlKeyFlags struct{ keys []string }

func (f *controlKeyFlags) addKey(raw string) error {
	key, err := sshkeys.Normalize(raw)
	if err != nil {
		return err
	}
	f.keys = mergeControlKeys(f.keys, []string{key})
	return nil
}
func (f *controlKeyFlags) addFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	// Normalize accepts one public key, not a private key or authorized_keys file.
	raw, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return fmt.Errorf("读取管理公钥文件: %w", err)
	}
	if len(raw) > 16384 {
		return fmt.Errorf("管理公钥文件过大")
	}
	return f.addKey(string(raw))
}
func mergeControlKeys(existing, additions []string) []string {
	result := append([]string{}, existing...)
	for _, key := range additions {
		if !slices.Contains(result, key) {
			result = append(result, key)
		}
	}
	return result
}
func validateControlKeys(keys []string) error {
	if keys == nil {
		return fmt.Errorf("管理公钥列表缺失")
	}
	seen := map[string]bool{}
	for _, key := range keys {
		normal, err := sshkeys.Normalize(key)
		if err != nil || normal != key || seen[key] {
			return fmt.Errorf("管理公钥列表包含无效或重复条目")
		}
		seen[key] = true
	}
	return nil
}
func marshalInstallation(c installation) ([]byte, error) {
	if c.Version != installationFormat {
		return nil, fmt.Errorf("安装格式不匹配，请使用新数据目录")
	}
	if err := validateControlKeys(c.ControlKeys); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if len(raw) > 16384 || len(controlAuthorizedKeys(c.ControlKeys)) > 16384 {
		return nil, fmt.Errorf("管理公钥列表过大，无法继续添加管理公钥")
	}
	return raw, err
}
func controlAuthorizedKeys(keys []string) []byte {
	var out bytes.Buffer
	for _, key := range keys {
		fmt.Fprintln(&out, "restrict "+key)
	}
	return out.Bytes()
}

// Called under the member-key lock to avoid using an outdated control-key list.
func readInstallation(r *os.Root, previous installation) (installation, error) {
	info, err := r.Stat(".")
	if err != nil {
		return previous, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return previous, fmt.Errorf("安装目录属主无效")
	}
	raw, err := readPrivateFile(r, "installation.json", int(owner.Uid), -1, 0022, 16384)
	if err != nil {
		return previous, err
	}
	var c installation
	if err = strictJSON(raw, &c); err != nil {
		return previous, err
	}
	if _, err = marshalInstallation(c); err != nil {
		return previous, err
	}
	if c.WorkerUID != previous.WorkerUID || c.WorkerGID != previous.WorkerGID || c.JumpUID != previous.JumpUID || c.JumpGID != previous.JumpGID {
		return previous, fmt.Errorf("安装账号身份已改变，请重试")
	}
	return c, nil
}
func lockKeyFiles(k *os.Root, c installation) (func(), error) {
	lock, err := k.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) { lock.Close(); return nil, err }
	info, err := lock.Stat()
	if err != nil {
		return fail(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || info.Mode().Perm() != 0600 {
		return fail(fmt.Errorf("公钥锁文件无效"))
	}
	// Initialization and local key additions also use the worker's key lock.
	if st.Uid == 0 && os.Geteuid() == 0 {
		if err = lock.Chown(c.WorkerUID, c.JumpGID); err != nil {
			return fail(err)
		}
	} else if int(st.Uid) != c.WorkerUID || int(st.Gid) != c.JumpGID {
		return fail(fmt.Errorf("公钥锁文件身份无效"))
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("公钥正在更新，请重试"))
	}
	return func() { syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); lock.Close() }, nil
}
func appendControlKeys(s keyStore, additions []string, write func(string, []byte, os.FileMode) error) error {
	if err := validateControlKeys(additions); err != nil {
		return err
	}
	r, c, err := s.open()
	if os.IsNotExist(err) {
		return fmt.Errorf("share node 尚未初始化，请先提供 --listen-host 和 --control-url 初始化")
	}
	if err != nil {
		return err
	}
	defer r.Close()
	k, err := openKeys(r, c)
	if err != nil {
		return err
	}
	defer k.Close()
	unlock, err := lockKeyFiles(k, c)
	if err != nil {
		return err
	}
	defer unlock()
	c, err = readInstallation(r, c)
	if err != nil {
		return err
	}
	old, err := marshalInstallation(c)
	if err != nil {
		return err
	}
	auth, err := readPrivateFile(r, "worker_authorized_keys", s.owner, -1, 0022, 16384)
	if err != nil {
		return err
	}
	if !bytes.Equal(auth, controlAuthorizedKeys(c.ControlKeys)) {
		return fmt.Errorf("管理授权文件与安装记录不一致，拒绝覆盖")
	}
	c.ControlKeys = mergeControlKeys(c.ControlKeys, additions)
	if _, err = loadSnapshot(k, c); err != nil {
		return err
	}
	updated, err := marshalInstallation(c)
	if err != nil {
		return err
	}
	if bytes.Equal(old, updated) {
		return nil
	}
	manifest := filepath.Join(s.path, "installation.json")
	if err = write(manifest, updated, 0644); err != nil {
		return err
	}
	if err = write(filepath.Join(s.path, "worker_authorized_keys"), controlAuthorizedKeys(c.ControlKeys), 0644); err != nil {
		return errors.Join(err, write(manifest, old, 0644))
	}
	return nil
}
