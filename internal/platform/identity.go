package platform

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"project-alpha/internal/httpapi"
)

// CheckMode verifies the identity written by the initialization transaction.
func (d *Database) CheckMode(mode string) (string, error) {
	var current, id string
	err := d.SQL.QueryRow("SELECT mode,instance_id FROM service_identity WHERE id=1").Scan(&current, &id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("data directory has no service identity; use a new data directory")
	}
	if err != nil {
		return "", err
	}
	if current != mode {
		return "", fmt.Errorf("data directory belongs to %s, cannot start %s; use a new data directory", current, mode)
	}
	return id, nil
}

// LockService prevents two services from recovering or scheduling the same work.
func (d *Database) LockService() (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(d.Directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("此数据目录已有管理服务正在运行: %w", err)
	}
	return lock, nil
}

type remoteUserKey struct{}

// WithRemoteUser is called only after authenticating the trusted control.
func WithRemoteUser(r *http.Request, user User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), remoteUserKey{}, user))
}

// RequestSession lets streaming modules recheck either a local session or a
// control-authenticated request. The control cancels streams on revocation.
func (d *Database) RequestSession(r *http.Request) (*Session, error) {
	if user, ok := r.Context().Value(remoteUserKey{}).(User); ok {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return &Session{User: user}, nil
	}
	return d.Session(httpapi.SessionToken(r))
}
