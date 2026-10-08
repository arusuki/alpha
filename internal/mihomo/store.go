package mihomo

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Store struct{ DB *platform.Database }
type Settings struct {
	DefaultTemplate string `json:"default_template"`
	Revision        int    `json:"revision"`
	BaseRevision    int    `json:"base_revision"`
	Inherited       bool   `json:"inherited"`
	Config          Config `json:"config"`
}
type SaveSettings struct {
	Revision     int    `json:"revision"`
	BaseRevision int    `json:"base_revision"`
	Inherit      bool   `json:"inherit"`
	Config       Config `json:"config"`
}

func (s Store) open(purpose, raw string, out any) error {
	plain, err := credentials.Decrypt(s.DB.Directory, "mihomo.key", "mihomo/"+purpose, raw)
	if err != nil {
		return fmt.Errorf("代理配置密钥或密文不可用；请恢复 mihomo.key 与数据库，或使用新数据目录：%w", err)
	}
	if err = json.Unmarshal([]byte(plain), out); err != nil {
		return fmt.Errorf("代理数据格式无效；请使用新数据目录")
	}
	return nil
}
func (s Store) seal(tx *sql.Tx, purpose string, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM mihomo_profiles WHERE config<>'' UNION ALL SELECT 1 FROM mihomo_sync WHERE bundle<>'' UNION ALL SELECT 1 FROM mihomo_runtime WHERE bundle<>'')`).Scan(&exists); err != nil {
		return "", err
	}
	encrypt := credentials.Encrypt
	if exists {
		encrypt = credentials.EncryptExisting
	}
	return encrypt(s.DB.Directory, "mihomo.key", "mihomo/"+purpose, string(raw))
}

func (s Store) Settings(target string) (Settings, error) {
	tx, err := s.DB.SQL.Begin()
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	return s.settings(tx, target)
}
func (s Store) settings(tx *sql.Tx, target string) (Settings, error) {
	v := Settings{Config: DefaultConfig(), DefaultTemplate: DefaultTemplate}
	var base string
	err := tx.QueryRow("SELECT revision,config FROM mihomo_profiles WHERE target='control'").Scan(&v.BaseRevision, &base)
	if err != nil && err != sql.ErrNoRows {
		return v, err
	}
	if base != "" {
		if err = s.open("profile/control", base, &v.Config); err != nil {
			return v, err
		}
	}
	if target == "control" {
		v.Revision = v.BaseRevision
		return v, nil
	}
	v.Inherited = true
	var raw string
	err = tx.QueryRow("SELECT revision,config FROM mihomo_profiles WHERE target=?", target).Scan(&v.Revision, &raw)
	if err != nil && err != sql.ErrNoRows {
		return v, err
	}
	if raw != "" {
		v.Inherited = false
		err = s.open("profile/"+target, raw, &v.Config)
		if err != nil {
			return v, err
		}
	}
	return v, nil
}

func (s Store) Save(target string, input SaveSettings, actor string) error {
	if target == "control" && input.Inherit {
		return httpapi.NewError(400, "总控配置不能继承自身")
	}
	if !input.Inherit {
		if err := input.Config.Validate(); err != nil {
			return httpapi.NewError(400, err.Error())
		}
	}
	return s.DB.Transaction(func(tx *sql.Tx) error {
		old, err := s.settings(tx, target)
		if err != nil {
			return err
		}
		if input.Revision != old.Revision || input.BaseRevision != old.BaseRevision {
			return httpapi.NewError(409, "代理配置已修改，请重新载入后再保存")
		}
		raw := ""
		if !input.Inherit {
			raw, err = s.seal(tx, "profile/"+target, input.Config)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO mihomo_profiles(target,revision,config,updated_at) VALUES(?,?,?,?) ON CONFLICT(target) DO UPDATE SET revision=excluded.revision,config=excluded.config,updated_at=excluded.updated_at`, target, old.Revision+1, raw, platform.Now())
		if err != nil {
			return err
		}
		return platform.Audit(tx, actor, "mihomo.settings", target)
	})
}

type SyncState struct {
	SourceVersion string  `json:"source_version"`
	RefreshedAt   float64 `json:"refreshed_at"`
	AttemptedAt   float64 `json:"attempted_at"`
	Delivered     bool    `json:"delivered"`
	Error         string  `json:"error"`
	Bundle        Bundle  `json:"-"`
}

func (s Store) Sync(target string) (SyncState, error) {
	v := SyncState{}
	var raw string
	err := s.DB.SQL.QueryRow("SELECT source_version,bundle,refreshed_at,attempted_at,delivered,error FROM mihomo_sync WHERE target=?", target).Scan(&v.SourceVersion, &raw, &v.RefreshedAt, &v.AttemptedAt, &v.Delivered, &v.Error)
	if err == sql.ErrNoRows {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if raw != "" {
		err = s.open("sync/"+target, raw, &v.Bundle)
	}
	return v, err
}
func (s Store) saveSync(target string, v SyncState) error {
	return s.DB.Transaction(func(tx *sql.Tx) error {
		raw := ""
		var err error
		if v.Bundle.Digest != "" {
			raw, err = s.seal(tx, "sync/"+target, v.Bundle)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO mihomo_sync(target,source_version,bundle,refreshed_at,attempted_at,delivered,error) VALUES(?,?,?,?,?,?,?) ON CONFLICT(target) DO UPDATE SET source_version=excluded.source_version,bundle=excluded.bundle,refreshed_at=excluded.refreshed_at,attempted_at=excluded.attempted_at,delivered=excluded.delivered,error=excluded.error`, target, v.SourceVersion, raw, v.RefreshedAt, v.AttemptedAt, v.Delivered, v.Error)
		return err
	})
}

type runtimeState struct {
	Bundle     Bundle
	Enabled    bool
	Selections map[string]string
}

func (s Store) runtime() (runtimeState, error) {
	v := runtimeState{Selections: map[string]string{}}
	var raw, selections string
	err := s.DB.SQL.QueryRow("SELECT bundle,enabled,selections FROM mihomo_runtime WHERE id=1").Scan(&raw, &v.Enabled, &selections)
	if err != nil {
		return v, err
	}
	if raw != "" {
		if err = s.open("runtime", raw, &v.Bundle); err != nil {
			return v, err
		}
	}
	if err = json.Unmarshal([]byte(selections), &v.Selections); err != nil || v.Selections == nil {
		return v, fmt.Errorf("代理选择记录格式无效；请使用新数据目录")
	}
	return v, nil
}
func (s Store) saveRuntime(v runtimeState, actor, action string) error {
	return s.DB.Transaction(func(tx *sql.Tx) error {
		raw := ""
		var err error
		if v.Bundle.Digest != "" {
			raw, err = s.seal(tx, "runtime", v.Bundle)
			if err != nil {
				return err
			}
		}
		selections, err := json.Marshal(v.Selections)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE mihomo_runtime SET bundle=?,enabled=?,selections=? WHERE id=1", raw, v.Enabled, string(selections)); err != nil {
			return err
		}
		return platform.Audit(tx, actor, action, "")
	})
}
