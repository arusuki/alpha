package updater

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var errMigrationUncertain = errors.New("database migration outcome requires inspection")

// Each executable is renamed atomically on the destination filesystem. Backups
// and the journal survive crashes; failures before a committed migration restore
// all replaced files. The database itself is never automatically overwritten.
func install(directory, stage string, files []binary, databaseBackup string, migrate func() error, out io.Writer) error {
	backupDir, err := os.MkdirTemp(directory, ".alpha-backup-")
	if err != nil {
		return err
	}
	existed := map[string]bool{}
	for _, file := range files {
		dest := filepath.Join(directory, file.destination)
		info, e := os.Lstat(dest)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file %s", dest)
		}
		if e = os.Link(dest, filepath.Join(backupDir, file.destination)); e != nil {
			return e
		}
		existed[file.destination] = true
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			if e = os.Chown(filepath.Join(stage, file.source), int(stat.Uid), int(stat.Gid)); e != nil {
				return e
			}
		}
	}
	if err = syncDirectory(backupDir); err != nil {
		return err
	}
	pending := filepath.Join(directory, ".alpha-update-pending")
	var journal strings.Builder
	fmt.Fprintf(&journal, "binary_backup=%s\ndatabase_backup=%s\n", backupDir, databaseBackup)
	for _, file := range files {
		fmt.Fprintf(&journal, "%s previous_file=%t\n", file.destination, existed[file.destination])
	}
	f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(journal.String())
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err = syncDirectory(directory); err != nil {
		return err
	}
	fmt.Fprintf(out, "二进制备份：%s\n", backupDir)
	var replaced []binary
	rollback := func(cause error) error {
		var failures []error
		for i := len(replaced) - 1; i >= 0; i-- {
			file := replaced[i]
			dest := filepath.Join(directory, file.destination)
			if existed[file.destination] {
				// Keep the backup link available after rollback as well.
				restore := filepath.Join(stage, "restore-"+file.destination)
				if e := os.Link(filepath.Join(backupDir, file.destination), restore); e != nil {
					failures = append(failures, e)
					continue
				}
				if e := os.Rename(restore, dest); e != nil {
					failures = append(failures, e)
				}
			} else if e := os.Remove(dest); e != nil {
				failures = append(failures, e)
			}
		}
		if e := syncDirectory(directory); e != nil {
			failures = append(failures, e)
		}
		if len(failures) != 0 {
			return fmt.Errorf("%w; binary rollback failed: %v; inspect %s before restarting", cause, errors.Join(failures...), pending)
		}
		if e := os.Remove(pending); e != nil {
			return errors.Join(cause, e)
		}
		if e := syncDirectory(directory); e != nil {
			return errors.Join(cause, e)
		}
		return fmt.Errorf("%w; previous binaries restored; database backup: %s", cause, databaseBackup)
	}
	for _, file := range files {
		if err = os.Rename(filepath.Join(stage, file.source), filepath.Join(directory, file.destination)); err != nil {
			return rollback(err)
		}
		replaced = append(replaced, file)
	}
	if err = syncDirectory(directory); err != nil {
		return rollback(err)
	}
	if err = migrate(); err != nil {
		if errors.Is(err, errMigrationUncertain) || errors.Is(err, errContainerUncertain) {
			return fmt.Errorf("%w; keep services stopped and inspect %s", err, pending)
		}
		return rollback(err)
	}
	// After migration succeeds, never restore older binaries automatically.
	if err = os.Remove(pending); err != nil {
		return fmt.Errorf("update committed but journal cleanup failed: %w", err)
	}
	if err = syncDirectory(directory); err != nil {
		return fmt.Errorf("update committed but directory sync failed: %w", err)
	}
	return nil
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
