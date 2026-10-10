// Package containers owns live Docker lifecycle management and explicit adoption.
package containers

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"project-alpha/internal/platform"
)

type Config struct {
	Endpoint  string `json:"endpoint"`
	Image     string `json:"image"`
	BaseDir   string `json:"base_dir"`
	StartPort int    `json:"start_port"`
	SSHHost   string `json:"ssh_host"`
	ProxyJump string `json:"proxy_jump"`
}

func defaults() Config {
	return Config{Endpoint: "unix:///var/run/docker.sock", BaseDir: "/docker", StartPort: 2222}
}

var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9.:[\]-]*$`)
var sshJumpPattern = regexp.MustCompile(`^[A-Za-z0-9_.:@,[\]%-]*$`)

func (c Config) validate() error {
	path := strings.TrimPrefix(c.Endpoint, "unix://")
	if !strings.HasPrefix(c.Endpoint, "unix://") || !cleanPath(path) {
		return fmt.Errorf("Docker endpoint 必须是本机 unix:// 套接字绝对路径")
	}
	if !cleanPath(c.BaseDir) || c.BaseDir == "/" || strings.ContainsAny(c.BaseDir, ",:") {
		return fmt.Errorf("数据根目录必须是非根目录的规范绝对路径，且不能包含逗号或冒号")
	}
	if c.StartPort < 1 || c.StartPort > 65535 {
		return fmt.Errorf("起始 SSH 端口必须为 1–65535")
	}
	if len(c.Image) > 512 || strings.HasPrefix(c.Image, "-") || strings.ContainsAny(c.Image, " \t\n\r\x00") {
		return fmt.Errorf("镜像名称无效")
	}
	for _, s := range []string{c.SSHHost, c.ProxyJump} {
		if strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n\x00") {
			return fmt.Errorf("SSH 主机和跳板机不能包含空白或以 - 开头")
		}
	}
	if !sshHostPattern.MatchString(c.SSHHost) || !sshJumpPattern.MatchString(c.ProxyJump) {
		return fmt.Errorf("SSH 主机或跳板机格式无效，不能含 shell 特殊字符")
	}
	return nil
}
func cleanPath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, "\x00\n\r?#%")
}

//go:embed schema.sql
var schema string

func Initialize(tx *sql.Tx) error {
	_, err := tx.Exec(schema)
	if err != nil {
		return err
	}
	if err = platform.InstallNodeServices(tx); err != nil {
		return err
	}
	raw, _ := json.Marshal(defaults())
	_, err = tx.Exec("INSERT INTO container_settings VALUES(1,?)", string(raw))
	return err
}

type Spec struct {
	Image      string `json:"image"`
	Network    string `json:"network"`
	Port       int    `json:"port"`
	GPUs       string `json:"gpus"`
	LocalProxy bool   `json:"local_proxy"`
	BaseDir    string `json:"base_dir"`
}
type Record struct {
	ID          string  `json:"id"`
	Endpoint    string  `json:"endpoint"`
	Daemon      string  `json:"daemon"`
	Name        string  `json:"name"`
	Owner       string  `json:"owner"`
	Spec        Spec    `json:"spec"`
	Fingerprint string  `json:"-"`
	Origin      string  `json:"origin"`
	Gate        string  `json:"-"`
	Initialized bool    `json:"initialized"`
	CreatedAt   float64 `json:"created_at"`
	State       string  `json:"state"`
	Error       string  `json:"error,omitempty"`
}

func (h *Handler) config() (Config, error) {
	var raw string
	var c Config
	err := h.db.SQL.QueryRow("SELECT value FROM container_settings WHERE id=1").Scan(&raw)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	return c, c.validate()
}
func (h *Handler) records() ([]Record, error) {
	rows, err := h.db.SQL.Query("SELECT id,endpoint,daemon,name,owner,spec,fingerprint,origin,gate,initialized,created_at FROM managed_containers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var raw string
		if err = rows.Scan(&r.ID, &r.Endpoint, &r.Daemon, &r.Name, &r.Owner, &raw, &r.Fingerprint, &r.Origin, &r.Gate, &r.Initialized, &r.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Spec); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (h *Handler) save(r Record, actor string) error {
	return h.db.Transaction(func(tx *sql.Tx) error { return h.saveTx(tx, r, actor) })
}

func (h *Handler) saveTx(tx *sql.Tx, r Record, actor string) error {
	raw, _ := json.Marshal(r.Spec)
	_, err := tx.Exec("INSERT INTO managed_containers(id,endpoint,daemon,name,owner,spec,fingerprint,origin,gate,initialized,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", r.ID, r.Endpoint, r.Daemon, r.Name, r.Owner, string(raw), r.Fingerprint, r.Origin, r.Gate, r.Initialized, platform.Now())
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE member_container_slots SET container_id=? WHERE member_id=? AND deleted=0 AND mode='create'", r.ID, strings.TrimPrefix(r.Name, "alpha-")); err != nil {
		return err
	}
	if h.Owner != nil {
		if err = h.Owner(tx, r.ID, r.Owner); err != nil {
			return err
		}
	}
	return platform.Audit(tx, actor, "container."+r.Origin, r.Name+" ("+r.ID+")")
}
