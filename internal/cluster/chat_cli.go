package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"project-alpha/internal/chatconfig"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type chatCLISettings struct {
	Revision int64 `json:"revision"`
	chatconfig.Settings
}

func (h *Control) chatCLISettings() (chatCLISettings, error) {
	var cfg chatCLISettings
	err := h.DB.SQL.QueryRow("SELECT revision,server,command_path FROM chat_cli_settings WHERE id=1").Scan(&cfg.Revision, &cfg.Server, &cfg.CommandPath)
	if err == nil {
		err = cfg.Settings.Validate()
	}
	return cfg, err
}

// Reuse the member provisioning queue: no additional timer or health check.
func queueChatCLI(tx *sql.Tx, memberID, nodeID string) error {
	_, err := tx.Exec(`INSERT INTO member_cli_sync(member_id,node_id,state,error,updated_at)
 SELECT m.id,n.id,'pending','',? FROM members m CROSS JOIN cluster_nodes n
 WHERE m.status='active' AND n.kind='worker' AND (?='' OR m.id=?) AND (?='' OR n.id=?)
 ON CONFLICT(member_id,node_id) DO UPDATE SET state='pending',error='',updated_at=excluded.updated_at,generation=member_cli_sync.generation+1`, platform.Now(), memberID, memberID, nodeID, nodeID)
	return err
}

func (h *Control) queueChatCLI(memberID, nodeID string) error {
	err := h.DB.Transaction(func(tx *sql.Tx) error { return queueChatCLI(tx, memberID, nodeID) })
	if err == nil {
		h.wakeProvision()
	}
	return err
}

func (h *Control) dispatchChatCLI(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "CLI 下发配置需要管理员权限")
	}
	cfg, err := h.chatCLISettings()
	if err != nil {
		return 0, nil, err
	}
	if r.URL.Path == "/api/assistant/cli" && r.Method == "GET" {
		rows, err := platform.Rows(h.DB.SQL, `SELECT s.member_id,m.username,s.node_id,n.name AS node_name,s.state,s.error,s.updated_at FROM member_cli_sync s JOIN members m ON m.id=s.member_id JOIN cluster_nodes n ON n.id=s.node_id ORDER BY m.username,n.name`)
		return 200, map[string]any{"settings": cfg, "deliveries": rows}, err
	}
	if r.URL.Path == "/api/assistant/cli" && r.Method == "PUT" {
		var next chatCLISettings
		if err := httpapi.DecodeBody(w, r, &next); err != nil {
			return 0, nil, err
		}
		if err := next.Settings.Validate(); err != nil {
			return 0, nil, httpapi.NewError(400, err.Error())
		}
		err = h.DB.Transaction(func(tx *sql.Tx) error {
			result, err := tx.Exec("UPDATE chat_cli_settings SET revision=revision+1,server=?,command_path=? WHERE id=1 AND revision=?", next.Server, next.CommandPath, next.Revision)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return httpapi.NewError(409, "CLI 配置已变化，请重新载入")
			}
			if err := queueChatCLI(tx, "", ""); err != nil {
				return err
			}
			return platform.Audit(tx, user.Username, "chat.cli.settings", next.CommandPath)
		})
	} else if r.URL.Path == "/api/assistant/cli/sync" && r.Method == "POST" {
		var body struct{}
		if err := httpapi.DecodeBody(w, r, &body); err != nil {
			return 0, nil, err
		}
		err = h.DB.Transaction(func(tx *sql.Tx) error {
			if err := queueChatCLI(tx, "", ""); err != nil {
				return err
			}
			return platform.Audit(tx, user.Username, "chat.cli.sync", "重新下发使用者 CLI")
		})
	} else {
		return 0, nil, httpapi.NewError(405, "不支持该 CLI 配置操作")
	}
	if err != nil {
		return 0, nil, err
	}
	h.wakeProvision()
	cfg, err = h.chatCLISettings()
	return 202, map[string]any{"settings": cfg}, err
}

func (h *Control) syncMemberCLI(ctx context.Context, id, username string) error {
	rows, err := platform.Rows(h.DB.SQL, "SELECT node_id,generation FROM member_cli_sync WHERE member_id=? AND state='pending'", id)
	if err != nil || len(rows) == 0 {
		return err
	}
	cfg, err := h.chatCLISettings()
	if err != nil {
		return err
	}
	server := cfg.Server
	if server == "" {
		access, e := h.Bastion.Access(id)
		err = e
		if e == nil {
			raw := memberStatusURL(access)
			if raw == "" {
				err = fmt.Errorf("尚无分享入口；请完成分享节点分配，或在 CLI 配置中填写容器可访问的总控地址")
			} else {
				u, e := url.Parse(raw)
				err = e
				if e == nil {
					u.Path = ""
					server = u.String()
				}
			}
		}
	}
	var failures []error
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nodeID := row["node_id"].(string)
		failure := err
		if failure == nil {
			node, e := h.node(nodeID)
			failure = e
			if e == nil {
				callCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
				var out struct {
					OK        bool     `json:"ok"`
					Installed int      `json:"installed"`
					Errors    []string `json:"errors"`
				}
				failure = h.call(callCtx, node, "PUT", "/api/containers/members/"+id+"/cli", map[string]string{"username": username, "server": server, "command_path": cfg.CommandPath}, platform.User{ID: id, Username: "chat-cli-control", Role: "admin"}, &out)
				cancel()
				if failure == nil && !out.OK {
					failure = fmt.Errorf("%s", strings.Join(out.Errors, "; "))
					if len(out.Errors) == 0 {
						failure = fmt.Errorf("worker 未确认 CLI 下发成功")
					}
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, message := "ready", ""
		if failure != nil {
			state, message = "failed", failure.Error()
		}
		_, persistErr := h.DB.SQL.Exec("UPDATE member_cli_sync SET state=?,error=?,updated_at=? WHERE member_id=? AND node_id=? AND generation=?", state, message, platform.Now(), id, nodeID, row["generation"])
		failures = append(failures, failure, persistErr)
	}
	return errors.Join(failures...)
}
