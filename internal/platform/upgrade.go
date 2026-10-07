package platform

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// OpenExistingDatabase never initializes an empty or missing data directory.
// Callers performing updates must hold LockService for the entire operation.
func OpenExistingDatabase(directory string, readOnly bool) (*Database, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "platform.sqlite3")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database must be a regular file: %s", path)
	}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?mode="+mode+"&_busy_timeout=15000&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Database{SQL: db, Directory: directory}, nil
}

// DatabaseState validates persisted identity and the supported upgrade range.
func (d *Database) DatabaseState() (version int, role string, err error) {
	if err = d.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if _, err = databaseUpgradePlan(version); err != nil {
		return
	}
	var id string
	err = d.SQL.QueryRow("SELECT mode,instance_id FROM service_identity WHERE id=1").Scan(&role, &id)
	if err == nil && (id == "" || (role != "control" && role != "worker" && role != "registry")) {
		err = fmt.Errorf("invalid service identity; existing data preserved")
	}
	return
}

// BackupDatabase uses SQLite's snapshot machinery so committed WAL pages are
// included. The destination must not exist; the source is never replaced.
func (d *Database) BackupDatabase(destination string) error {
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("backup already exists: %s", destination)
	}
	if _, err := d.SQL.Exec("VACUUM INTO ?", destination); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0600); err != nil {
		return err
	}
	f, err := os.Open(destination)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// UpgradeDatabase uses the same transaction as normal startup, but never creates
// a database. A supplied lock can be inherited by a release's migration process.
func UpgradeDatabase(directory, role string, lock *os.File) error {
	db, err := OpenExistingDatabase(directory, false)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	if lock == nil {
		lock, err = db.LockService()
		if err != nil {
			return err
		}
		defer lock.Close()
	} else {
		actual, e := lock.Stat()
		if e != nil {
			return e
		}
		expected, e := os.Stat(filepath.Join(db.Directory, "service.lock"))
		if e != nil {
			return e
		}
		if !os.SameFile(actual, expected) {
			return fmt.Errorf("inherited lock does not belong to this data directory")
		}
		if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			return e
		}
	}
	version, current, err := db.DatabaseState()
	if err != nil {
		return err
	}
	if role != current {
		return fmt.Errorf("data directory belongs to %s, not %s", current, role)
	}
	return db.Transaction(func(tx *sql.Tx) error { return upgradeDatabase(tx, version) })
}

func upgradeDatabase(tx *sql.Tx, version int) error {
	plan, err := databaseUpgradePlan(version)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		return nil
	}
	for _, step := range plan {
		if err = step.Apply(tx); err != nil {
			return fmt.Errorf("database upgrade %d → %d failed; existing data preserved: %w", step.From, step.To, err)
		}
	}
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	broken := rows.Next()
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if broken {
		return fmt.Errorf("database upgrade foreign key check failed; existing data preserved")
	}
	var integrity string
	if err = tx.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("database upgrade integrity check failed: %s", integrity)
	}
	if err = Audit(tx, "alpha-updater", "database.upgrade", fmt.Sprintf("%d -> %d", version, DatabaseVersion)); err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", DatabaseVersion))
	return err
}
