package agent

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Config struct {
	Protocol         string `json:"protocol"`
	Endpoint         string `json:"endpoint"`
	Model            string `json:"model"`
	APIKey           string `json:"api_key"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	ReasoningSummary bool   `json:"reasoning_summary"`
}

func defaultConfig() Config {
	return Config{Protocol: "responses", Endpoint: "https://api.openai.com/v1", TimeoutSeconds: 180}
}

func (c Config) endpointURL() (string, error) {
	if c.Protocol != "responses" && c.Protocol != "completions" {
		return "", httpapi.NewError(400, "接口必须为 responses 或 completions")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(c.Endpoint) > 2048 {
		return "", httpapi.NewError(400, "Endpoint 必须是 HTTP(S) 地址，不能包含账号、查询参数或片段")
	}
	suffix := "/responses"
	if c.Protocol == "completions" {
		suffix = "/chat/completions"
	}
	p := strings.TrimRight(u.Path, "/")
	for _, ending := range []string{"/chat/completions", "/responses"} {
		if strings.HasSuffix(p, ending) {
			p = strings.TrimSuffix(p, ending)
			break
		}
	}
	if p == "" {
		p = "/v1"
	}
	u.Path, u.RawPath = p+suffix, ""
	return u.String(), nil
}

func (c Config) validate() error {
	if _, err := c.endpointURL(); err != nil {
		return err
	}
	if len(c.Model) > 200 || strings.ContainsAny(c.Model, "\r\n") || len(c.APIKey) > 8192 || strings.ContainsAny(c.APIKey, "\r\n") {
		return httpapi.NewError(400, "模型名或 API Key 格式无效")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 600 {
		return httpapi.NewError(400, "单次模型请求超时需为 10–600 秒")
	}
	return nil
}

func (d *Store) agentConfig() (Config, int64, error) {
	var raw string
	var revision int64
	var c Config
	err := d.SQL.QueryRow("SELECT value,revision FROM agent_settings WHERE id=1").Scan(&raw, &revision)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c)
	}
	return c, revision, err
}

func publicConfig(c Config, revision int64) object {
	return object{"revision": revision, "value": object{"protocol": c.Protocol, "endpoint": c.Endpoint, "model": c.Model, "timeout_seconds": c.TimeoutSeconds, "has_api_key": c.APIKey != "", "reasoning_summary": c.ReasoningSummary}}
}

func (d *Store) saveConfig(fields map[string]json.RawMessage, actor string) (object, error) {
	for k := range fields {
		if k != "revision" && k != "value" {
			return nil, httpapi.NewError(400, "未知配置字段")
		}
	}
	var revision int64
	if json.Unmarshal(fields["revision"], &revision) != nil || revision < 1 {
		return nil, httpapi.NewError(400, "缺少配置版本")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(fields["value"], &value) != nil || value == nil {
		return nil, httpapi.NewError(400, "模型配置无效")
	}
	c, _, err := d.agentConfig()
	if err != nil {
		return nil, err
	}
	for k, v := range value {
		if string(v) == "null" {
			return nil, httpapi.NewError(400, "模型配置字段不能为 null")
		}
		switch k {
		case "protocol":
			err = json.Unmarshal(v, &c.Protocol)
		case "endpoint":
			err = json.Unmarshal(v, &c.Endpoint)
		case "model":
			err = json.Unmarshal(v, &c.Model)
		case "reasoning_summary":
			err = json.Unmarshal(v, &c.ReasoningSummary)
		case "timeout_seconds":
			err = json.Unmarshal(v, &c.TimeoutSeconds)
		case "api_key":
			var key string
			err = json.Unmarshal(v, &key)
			if strings.TrimSpace(key) != "" {
				c.APIKey = strings.TrimSpace(key)
			}
		case "clear_api_key": // Processed after api_key, independent of map iteration order.
		default:
			return nil, httpapi.NewError(400, "未知模型配置字段")
		}
		if err != nil {
			return nil, httpapi.NewError(400, "模型配置字段类型无效")
		}
	}
	if v, ok := value["clear_api_key"]; ok {
		var clear bool
		if json.Unmarshal(v, &clear) != nil {
			return nil, httpapi.NewError(400, "清除 API Key 参数无效")
		}
		if clear {
			c.APIKey = ""
		}
	}
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Model = strings.TrimSpace(c.Model)
	if err = c.validate(); err != nil {
		return nil, err
	}
	err = d.Transaction(func(tx *sql.Tx) error {
		r, err := tx.Exec("UPDATE agent_settings SET value=?,revision=revision+1 WHERE id=1 AND revision=?", httpapi.JSONText(c), revision)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(409, "模型配置已被修改，请重新载入")
		}
		return platform.Audit(tx, actor, "agent.settings", c.Protocol+" / "+c.Model)
	})
	if err != nil {
		return nil, err
	}
	return publicConfig(c, revision+1), nil
}
