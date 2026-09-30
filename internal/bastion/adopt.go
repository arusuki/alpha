package bastion

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The caller holds both the installation and old publisher locks. Prepare a
// complete new installation, then exchange directories atomically so readers
// never see a manifest paired with another control's snapshot or ownership.
// The old tree stays intact as the archive; this is a transfer of management,
// not a conversion of persisted formats or a rewrite of the old control DB.
func transferInstallation(source, target installation, snapshot keySnapshot) (string, error) {
	if source.JumpUID != target.JumpUID || source.JumpGID != target.JumpGID || source.AccountRemoved || target.ServiceUID == target.JumpUID {
		return "", fmt.Errorf("接管不能改变 alpha-jump 的系统身份")
	}
	parent := filepath.Dir(installationDirectory)
	stage, err := os.MkdirTemp(parent, "project-alpha-jump.before-adopt-")
	if err != nil {
		return "", err
	}
	exchanged := false
	defer func() {
		if !exchanged {
			// Only this newly created staging tree is disposable. After the
			// exchange this path contains the original data and must be kept.
			_ = os.RemoveAll(stage)
		}
	}()
	keys := filepath.Join(stage, "keys")
	if err = os.Mkdir(keys, 0700); err != nil {
		return "", err
	}
	snapshot.ControlID = target.ControlID
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	for name, content := range map[string][]byte{"keys.json": raw, ".lock": nil} {
		mode := os.FileMode(0640)
		if name == ".lock" {
			mode = 0600
		}
		path := filepath.Join(keys, name)
		if err = rootFile(path, content, mode); err != nil {
			return "", err
		}
		if err = os.Chown(path, target.ServiceUID, target.JumpGID); err != nil {
			return "", err
		}
	}
	if err = os.Chown(keys, target.ServiceUID, target.JumpGID); err != nil {
		return "", err
	}
	if err = os.Chmod(keys, os.ModeSetgid|0750); err != nil {
		return "", err
	}
	raw, err = json.Marshal(target)
	if err != nil {
		return "", err
	}
	if err = rootFile(filepath.Join(stage, "installation.json"), raw, 0644); err != nil {
		return "", err
	}
	// Preserve the installation lock inode across exchanges. An installer
	// that opened the old directory must serialize with one opening the new.
	if err = os.Link(filepath.Join(installationDirectory, ".install.lock"), filepath.Join(stage, ".install.lock")); err != nil {
		return "", err
	}
	if err = os.Chmod(stage, 0755); err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Join(keys, "keys.json"), filepath.Join(keys, ".lock"), keys, stage} {
		if err = syncAdoptionPath(path); err != nil {
			return "", err
		}
	}
	if err = unix.Renameat2(unix.AT_FDCWD, installationDirectory, unix.AT_FDCWD, stage, unix.RENAME_EXCHANGE); err != nil {
		return "", fmt.Errorf("切换跳板 data 失败，原账号、绑定和 data 保留: %w", err)
	}
	exchanged = true
	if err = syncAdoptionPath(parent); err != nil {
		return stage, fmt.Errorf("接管已切换，目录同步失败，请刷新状态核对；原 data 在 %s: %w", stage, err)
	}
	return stage, nil
}

func syncAdoptionPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
