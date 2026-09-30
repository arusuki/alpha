package platform

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"

	"project-alpha/internal/fsutil"
)

// DatabaseVersion identifies the combined schema and persisted event formats.
const DatabaseVersion = 30

//go:embed schema.sql
var schema string

type Database struct {
	SQL       *sql.DB
	Directory string
}
type Queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}
type executor interface {
	Exec(string, ...any) (sql.Result, error)
}

func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// OpenDatabase initializes new data with platform and module tables in one transaction.
func OpenDatabase(directory string, initialize func(*sql.Tx) error) (*Database, error) {
	directory = fsutil.Canonical(directory)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "platform.sqlite3")
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=15000&_foreign_keys=on&_journal_mode=WAL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Database, error) { db.Close(); return nil, err }
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	switch version {
	case 0:
		if _, err = tx.Exec(schema); err != nil {
			return fail(err)
		}
		if initialize != nil {
			if err = initialize(tx); err != nil {
				return fail(err)
			}
		}
		if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", DatabaseVersion)); err != nil {
			return fail(err)
		}
	case DatabaseVersion:
	default:
		return fail(fmt.Errorf("unsupported database version %d; expected %d; use a new data directory", version, DatabaseVersion))
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err)
	}
	return &Database{db, directory}, nil
}
func Rows(q Queryer, query string, args ...any) ([]object, error) {
	rs, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	out := []object{}
	for rs.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rs.Scan(pointers...); err != nil {
			return nil, err
		}
		row := object{}
		for i, col := range cols {
			if b, ok := values[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = values[i]
			}
		}
		out = append(out, row)
	}
	return out, rs.Err()
}
func Audit(e executor, actor, action, detail string) error {
	_, err := e.Exec("INSERT INTO audit(at,actor,action,detail) VALUES(?,?,?,?)", Now(), actor, action, detail)
	return err
}
func (d *Database) Transaction(fn func(*sql.Tx) error) error {
	tx, err := d.SQL.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func IsConstraint(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && e.Code == sqlite3.ErrConstraint
}
