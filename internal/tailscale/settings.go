// Package tailscale connects the platform to the Tailscale management API.
package tailscale

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"

	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

//go:embed schema.sql
var schema string

func Initialize(tx *sql.Tx) error { _, err := tx.Exec(schema); return err }

const tokenKeyFile = "tailscale-api-token.key"
const tokenAAD = "project-alpha/tailscale-settings/api-token/v1"

type Settings struct {
	Revision    int64  `json:"revision"`
	Tailnet     string `json:"tailnet"`
	HasAPIToken bool   `json:"has_api_token"`
}

func (h *Handler) settings() (Settings, string, error) {
	var s Settings
	var ciphertext string
	err := h.db.SQL.QueryRow("SELECT revision,tailnet,token_ciphertext FROM tailscale_settings WHERE id=1").Scan(&s.Revision, &s.Tailnet, &ciphertext)
	s.HasAPIToken = ciphertext != ""
	return s, ciphertext, err
}

func (h *Handler) decrypt(ciphertext string) (string, error) {
	token, err := credentials.Decrypt(h.db.Directory, tokenKeyFile, tokenAAD, ciphertext)
	if err != nil {
		return "", httpapi.NewError(500, "无法解密 Tailscale 凭据；请检查或恢复数据目录中的 "+tokenKeyFile+"（权限须为 0600），或使用新数据目录")
	}
	return token, nil
}

var tailnetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.@_-]{0,252}$`)
var tokenPattern = regexp.MustCompile(`^tskey-api-[A-Za-z0-9_-]+$`)

func (h *Handler) saveSettings(fields map[string]json.RawMessage, actor string) (Settings, error) {
	var req struct {
		Revision      int64
		Tailnet       string
		APIToken      string
		ClearAPIToken bool
	}
	for k, v := range fields {
		var target any
		switch k {
		case "revision":
			target = &req.Revision
		case "tailnet":
			target = &req.Tailnet
		case "api_token":
			target = &req.APIToken
		case "clear_api_token":
			target = &req.ClearAPIToken
		default:
			return Settings{}, httpapi.NewError(400, "未知 Tailscale 配置字段")
		}
		if string(v) == "null" || json.Unmarshal(v, target) != nil {
			return Settings{}, httpapi.NewError(400, "Tailscale 配置字段类型无效")
		}
	}
	req.Tailnet, req.APIToken = strings.TrimSpace(req.Tailnet), strings.TrimSpace(req.APIToken)
	if req.Revision < 1 || (req.Tailnet != "-" && !tailnetPattern.MatchString(req.Tailnet)) {
		return Settings{}, httpapi.NewError(400, "需提供配置版本和有效的 Tailnet ID 或名称；当前凭据所属网络可填 -")
	}
	if req.APIToken != "" && (len(req.APIToken) > 8192 || !tokenPattern.MatchString(req.APIToken)) {
		return Settings{}, httpapi.NewError(400, "请填写以 tskey-api- 开头的个人 API Token")
	}
	if req.ClearAPIToken && req.APIToken != "" {
		return Settings{}, httpapi.NewError(400, "不能同时替换和清除 API Token")
	}
	s, ciphertext, err := h.settings()
	if err != nil {
		return s, err
	}
	if s.Revision != req.Revision {
		return s, httpapi.NewError(409, "Tailscale 配置已被修改，请重新载入")
	}
	if req.ClearAPIToken {
		ciphertext = ""
	} else {
		// Never generate a replacement encryption key for existing ciphertext.
		if ciphertext != "" {
			if _, err = h.decrypt(ciphertext); err != nil {
				return s, err
			}
		}
		if req.APIToken != "" {
			ciphertext, err = credentials.Encrypt(h.db.Directory, tokenKeyFile, tokenAAD, req.APIToken)
			if err != nil {
				return s, err
			}
		}
	}
	err = h.db.Transaction(func(tx *sql.Tx) error {
		r, e := tx.Exec("UPDATE tailscale_settings SET tailnet=?,token_ciphertext=?,revision=revision+1 WHERE id=1 AND revision=?", req.Tailnet, ciphertext, req.Revision)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return httpapi.NewError(409, "Tailscale 配置已被修改，请重新载入")
		}
		return platform.Audit(tx, actor, "tailscale.settings", "修改 Tailscale 网络或凭据")
	})
	return Settings{Revision: req.Revision + 1, Tailnet: req.Tailnet, HasAPIToken: ciphertext != ""}, err
}
