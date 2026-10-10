package updater

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var errUpdaterUncertain = errors.New("configured updater rollback requires inspection")

// The target owns this plan; the bootstrap and service retain its opaque JSON.
type configuredUpdater struct {
	Path, Hash string
}

func canonicalDestination(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("executable path must be absolute: %s", path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

func prepareConfiguredUpdater(command, binDir, role string) (*configuredUpdater, error) {
	if command == "" {
		return nil, nil
	}
	path, err := canonicalDestination(command)
	if err != nil {
		return nil, err
	}
	for _, file := range binaries(role) {
		destination, err := canonicalDestination(filepath.Join(binDir, file.destination))
		if err != nil {
			return nil, err
		}
		if path == destination {
			if file.source == "alpha-updater" {
				return nil, nil
			}
			return nil, fmt.Errorf("configured updater conflicts with %s", destination)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("expected a regular executable: %s", path)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if parent.Mode()&os.ModeSticky != 0 && os.Geteuid() != 0 {
		owner := info.Sys().(*syscall.Stat_t).Uid
		dirOwner := parent.Sys().(*syscall.Stat_t).Uid
		if uint32(os.Geteuid()) != owner && uint32(os.Geteuid()) != dirOwner {
			return nil, fmt.Errorf("cannot replace configured updater in sticky directory: %s", path)
		}
	}
	// Exercise creation and backup permissions while the service is still up.
	probe, err := os.MkdirTemp(filepath.Dir(path), ".alpha-updater-check-")
	if err != nil {
		return nil, fmt.Errorf("configured updater directory is not writable: %w", err)
	}
	defer os.RemoveAll(probe)
	if err := os.Link(path, filepath.Join(probe, "previous")); err != nil {
		return nil, fmt.Errorf("cannot back up configured updater: %w", err)
	}
	hash, err := fileHash(path)
	if err != nil {
		return nil, err
	}
	return &configuredUpdater{Path: path, Hash: hash}, nil
}

func (p *configuredUpdater) lock(binDir string) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	dir, err := filepath.EvalSymlinks(binDir)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(p.Path) == dir {
		return func() {}, nil // Already held by the binary installer.
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(p.Path), ".alpha-updater.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another updater is using the configured updater directory: %w", err)
	}
	return func() { f.Close() }, nil
}

// Run within install's journal, before containers and the database commit. Use
// the destination filesystem for both the backup and atomic replacements.
func (p *configuredUpdater) apply(binDir string, out io.Writer) (func() error, error) {
	if p == nil {
		return func() error { return nil }, nil
	}
	hash, err := fileHash(p.Path)
	if err != nil {
		return nil, err
	}
	if hash != p.Hash {
		return nil, errors.New("configured updater changed after preparation; prepare again")
	}
	info, err := os.Lstat(p.Path)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(p.Path), ".alpha-updater-backup-")
	if err != nil {
		return nil, err
	}
	journaled := false
	defer func() {
		if !journaled {
			os.RemoveAll(dir)
		}
	}()
	backup := filepath.Join(dir, "previous")
	if err := os.Link(p.Path, backup); err != nil {
		return nil, err
	}
	src, err := os.Open(filepath.Join(binDir, "alpha-updater"))
	if err != nil {
		return nil, err
	}
	defer src.Close()
	next := filepath.Join(dir, "next")
	f, err := os.OpenFile(next, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(f, src)
	var ownerErr error
	if os.Geteuid() == 0 {
		stat := info.Sys().(*syscall.Stat_t)
		ownerErr = f.Chown(int(stat.Uid), int(stat.Gid))
	}
	err = errors.Join(copyErr, ownerErr, f.Chmod(info.Mode().Perm()), f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(p.Path)); err != nil {
		return nil, err
	}
	journal, err := os.OpenFile(filepath.Join(binDir, ".alpha-update-pending"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := fmt.Fprintf(journal, "configured_updater=%q\nconfigured_updater_backup=%q\n", p.Path, backup)
	journaled = true
	if err := errors.Join(writeErr, journal.Sync(), journal.Close()); err != nil {
		return nil, err
	}
	rollback := func() error {
		restore := filepath.Join(dir, "restore")
		err := os.Link(backup, restore)
		if err == nil {
			err = os.Rename(restore, p.Path)
		}
		if err == nil {
			err = syncDirectory(filepath.Dir(p.Path))
		}
		if err != nil {
			return fmt.Errorf("%w: %v; restore %s from %s", errUpdaterUncertain, err, p.Path, backup)
		}
		return nil
	}
	if err := os.Rename(next, p.Path); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(p.Path)); err != nil {
		return nil, errors.Join(err, rollback())
	}
	fmt.Fprintf(out, "已更新配置的 updater：%s；备份：%s\n", p.Path, backup)
	return rollback, nil
}
