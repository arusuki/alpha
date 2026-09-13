package storage

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Store persists storage settings, jobs, snapshots and analysis sessions.
type Store struct{ *platform.Database }

func NewStore(db *platform.Database) *Store { return &Store{Database: db} }

//go:embed schema.sql
var schema string

// Initialize creates the storage module's tables and defaults in the platform transaction.
func Initialize(tx *sql.Tx) error {
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO settings(id,value) VALUES(1,?)", httpapi.JSONText(defaultConfig())); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO agent_settings(id,value) VALUES(1,?)", httpapi.JSONText(defaultAgentConfig()))
	return err
}

type Settings struct {
	Value    Config `json:"value"`
	Revision int64  `json:"revision"`
}

func (d *Store) config() (Settings, error) {
	var s Settings
	var raw string
	err := d.SQL.QueryRow("SELECT value,revision FROM settings WHERE id=1").Scan(&raw, &s.Revision)
	if err != nil {
		return s, err
	}
	s.Value, err = parseConfig([]byte(raw))
	return s, err
}
func (d *Store) saveConfig(c Config, revision int64, actor string) (Settings, error) {
	if err := c.validate(); err != nil {
		return Settings{}, err
	}
	err := d.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE settings SET value=?,revision=revision+1 WHERE id=1 AND revision=?", httpapi.JSONText(c), revision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(409, "配置已被其他管理员修改，请重新载入后保存")
		}
		return platform.Audit(tx, actor, "settings.update", httpapi.JSONText(c))
	})
	if err != nil {
		return Settings{}, err
	}
	return Settings{c, revision + 1}, nil
}
func decodeJobs(items []object) error {
	for _, item := range items {
		for _, key := range []string{"config", "progress"} {
			var value any
			if err := json.Unmarshal([]byte(item[key].(string)), &value); err != nil {
				return err
			}
			item[key] = value
		}
	}
	return nil
}

const jobProjection = "SELECT jobs.*,coalesce(h.revision,0) AS snapshot_revision FROM jobs LEFT JOIN snapshot_records h ON h.job_id=jobs.id"

func (d *Store) job(id string) (object, error) {
	items, err := platform.Rows(d.SQL, jobProjection+" WHERE jobs.id=?", id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, httpapi.NewError(404, "扫描任务不存在")
	}
	if err = decodeJobs(items); err != nil {
		return nil, err
	}
	return items[0], nil
}
func (d *Store) jobs(before float64) ([]object, error) {
	items, err := platform.Rows(d.SQL, jobProjection+" WHERE created_at<? AND trigger<>'incremental' ORDER BY created_at DESC LIMIT 50", before)
	if err != nil {
		return nil, err
	}
	return items, decodeJobs(items)
}
func (d *Store) directoryJobs() ([]object, error) {
	items, err := platform.Rows(d.SQL, "SELECT * FROM jobs WHERE trigger='incremental' ORDER BY created_at DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	return items, decodeJobs(items)
}
func (d *Store) latest() (*string, error) {
	var id string
	err := d.SQL.QueryRow("SELECT id FROM jobs WHERE status='completed' AND trigger NOT IN ('agent-detail','incremental') ORDER BY finished_at DESC LIMIT 1").Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &id, err
}
func (d *Store) owners() (map[string]string, error) {
	items, err := platform.Rows(d.SQL, "SELECT * FROM owners")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range items {
		out[r["container_id"].(string)] = r["owner"].(string)
	}
	return out, nil
}
func (d *Store) setOwner(cid, owner, actor string) error {
	if !containerPattern.MatchString(cid) || utf8.RuneCountInString(owner) > 100 {
		return httpapi.NewError(400, "容器标识或所属用户无效")
	}
	return d.Transaction(func(tx *sql.Tx) error {
		var err error
		if strings.TrimSpace(owner) != "" {
			_, err = tx.Exec("INSERT INTO owners VALUES(?,?) ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner", cid, strings.TrimSpace(owner))
		} else {
			_, err = tx.Exec("DELETE FROM owners WHERE container_id=?", cid)
		}
		if err != nil {
			return err
		}
		return platform.Audit(tx, actor, "container.owner", cid+" / "+owner)
	})
}
