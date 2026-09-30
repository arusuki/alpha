package bastion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func validAccountAction(action string) bool {
	return action == "init" || action == "adopt" || action == "release" || action == "delete"
}

func checkInstallationBinding(c installation, id, service string, uid int, action string) error {
	if action != "adopt" && (c.ControlID != id || c.ServiceUID != uid || c.ServiceUser != service) {
		return fmt.Errorf("alpha-jump 当前由其他 control 或服务用户管理，请选择接管已有账号，将账号、工具和公钥 data 转交当前 control")
	}
	if action == "init" && c.Released {
		return fmt.Errorf("alpha-jump 已取消接管，请选择接管已有账号")
	}
	if (action == "adopt" || action == "release") && c.AccountRemoved {
		return fmt.Errorf("alpha-jump 账号已删除，请选择添加账号")
	}
	return nil
}

func saveInstallation(c installation) error {
	// Set stop markers before updating the reader's Ready flag. Clear them
	// only after its manifest is durable, so interrupted work stays blocked.
	flags := map[string]bool{"released": c.Released, "account-removed": c.AccountRemoved}
	for name, active := range flags {
		if active {
			if err := rootFile(filepath.Join(installationDirectory, name), []byte(name+"\n"), 0644); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err = rootFile(filepath.Join(installationDirectory, "installation.json"), raw, 0644); err != nil {
		return err
	}
	for name, active := range flags {
		if !active {
			if err = os.Remove(filepath.Join(installationDirectory, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// Share the publisher's lock so no in-flight publication can outlive release
// or add keys between the deletion check and userdel.
func lockInstallationKeys(k *os.Root, c installation) (*os.File, error) {
	f, err := k.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { f.Close(); return nil, err }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || info.Mode().Perm() != 0600 || (int(st.Uid) != c.ServiceUID && st.Uid != 0) {
		return fail(fmt.Errorf("公钥清单锁文件无效"))
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("公钥清单正在更新，请重试"))
	}
	if err = f.Chown(c.ServiceUID, c.JumpGID); err != nil {
		return fail(err)
	}
	return f, nil
}

func manageAccount(ctx context.Context, action string, c installation, snapshot keySnapshot, out io.Writer) error {
	if action == "release" {
		if err := rootFile(filepath.Join(installationDirectory, "released"), []byte("released\n"), 0644); err != nil {
			return err
		}
		fmt.Fprintln(out, "已取消接管 alpha-jump；账号、工具、data、SSH 配置和现有授权均保留，平台停止公钥管理。")
		return nil
	}
	if len(snapshot.Keys) != 0 {
		return fmt.Errorf("仍有 %d 条跳板公钥，请先撤销全部公钥再删除账号；工具和 data 将保留", len(snapshot.Keys))
	}
	previous := c
	c.Ready, c.Released, c.AccountRemoved = false, false, true
	// Persist intent first: an interrupted deletion remains fail-closed and
	// retryable even if userdel has already removed the account.
	if err := saveInstallation(c); err != nil {
		return err
	}
	if jump, err := user.Lookup(JumpUser); err == nil {
		if jump.Uid != strconv.Itoa(c.JumpUID) || jump.Gid != strconv.Itoa(c.JumpGID) {
			return fmt.Errorf("alpha-jump 身份已改变，拒绝删除")
		}
		if _, err = runInstall(ctx, "/usr/sbin/userdel", JumpUser); err != nil {
			remaining, lookupErr := user.Lookup(JumpUser)
			if remaining != nil && remaining.Uid == strconv.Itoa(c.JumpUID) {
				if restoreErr := saveInstallation(previous); restoreErr != nil {
					return fmt.Errorf("删除失败，恢复管理状态失败: %v；%w", restoreErr, err)
				}
				return fmt.Errorf("删除账号失败（不强制终止进程），请核对账号占用后重试: %w", err)
			}
			if _, absent := lookupErr.(user.UnknownUserError); !absent {
				return fmt.Errorf("删除结果无法确认，请刷新核对: %w", err)
			}
		}
	} else if _, absent := err.(user.UnknownUserError); !absent {
		return err
	}
	fmt.Fprintln(out, "已删除 alpha-jump 账号；工具、data、home 和 SSH 配置保留，可重新添加账号。")
	return nil
}

// Reuse the recorded numeric identity; never silently assign retained data to
// another user or change ownership recursively through a service-writable tree.
func recreateAccount(ctx context.Context, c installation) error {
	if jump, err := user.Lookup(JumpUser); err == nil {
		if jump.Uid != strconv.Itoa(c.JumpUID) || jump.Gid != strconv.Itoa(c.JumpGID) {
			return fmt.Errorf("alpha-jump 身份已改变，拒绝复用已有 data")
		}
		return nil
	} else if _, absent := err.(user.UnknownUserError); !absent {
		return err
	}
	if _, err := user.LookupId(strconv.Itoa(c.JumpUID)); err == nil {
		return fmt.Errorf("原 alpha-jump UID 已被占用，保留 data，请先核对账号")
	} else if _, absent := err.(user.UnknownUserIdError); !absent {
		return err
	}
	if _, err := user.LookupGroupId(strconv.Itoa(c.JumpGID)); err != nil {
		if _, absent := err.(user.UnknownGroupIdError); !absent {
			return err
		}
		if _, err = runInstall(ctx, "/usr/sbin/groupadd", "--system", "--gid", strconv.Itoa(c.JumpGID), JumpUser); err != nil {
			return err
		}
	}
	if err := rootDirectory(jumpHome, 0755); err != nil {
		return err
	}
	_, err := runInstall(ctx, "/usr/sbin/useradd", "--system", "--uid", strconv.Itoa(c.JumpUID), "--gid", strconv.Itoa(c.JumpGID), "--home-dir", jumpHome, "--no-create-home", "--shell", "/bin/false", "--password", "*", JumpUser)
	return err
}

type accountInfo struct {
	Exists   bool `json:"exists"`
	Managed  bool `json:"managed"`
	Released bool `json:"released"`
	Removed  bool `json:"removed"`
}

func (s keyStore) accountInfo(id string) accountInfo {
	_, err := s.lookup(JumpUser)
	v := accountInfo{Exists: err == nil}
	r, c, err := s.open()
	if err != nil {
		return v
	}
	r.Close()
	if c.ControlID == id && c.ServiceUID == os.Geteuid() {
		v.Managed = !c.Released && !c.AccountRemoved
		v.Released, v.Removed = c.Released, c.AccountRemoved
	}
	return v
}
