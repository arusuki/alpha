// Package bastion tracks shared network access and annotated local SSH keys.
package bastion

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/tailscale"
)

//go:embed schema.sql
var schema string

func Initialize(tx *sql.Tx) error {
	if err := tailscale.Initialize(tx); err != nil {
		return err
	}
	_, err := tx.Exec(schema)
	return err
}

type Network interface {
	platform.Module
	Devices(context.Context) ([]tailscale.Device, error)
	Invites(context.Context, string) ([]tailscale.Invite, error)
	CreateInvite(context.Context, string) (tailscale.Invite, error)
	DeleteInvite(context.Context, string) error
}

type Handler struct {
	DB        *platform.Database
	Tailscale Network
	mu        sync.Mutex
	KeyEditor func(Account, string, string) error
}

func NewHandler(db *platform.Database) *Handler {
	return &Handler{DB: db, Tailscale: tailscale.NewHandler(db), KeyEditor: editKey}
}
func IsRoute(path string) bool {
	return strings.HasPrefix(path, "/api/bastion/") || tailscale.IsRoute(path)
}
func (h *Handler) Reserve(tx *sql.Tx, m members.Member) error {
	if _, err := tx.Exec("INSERT INTO member_access(member_id,updated_at) VALUES(?,?)", m.ID, platform.Now()); err != nil {
		return err
	}
	return allocate(tx, m.ID)
}
func allocate(tx *sql.Tx, id string) error {
	_, err := tx.Exec(`UPDATE member_access SET tailscale_id=COALESCE(tailscale_id,(SELECT b.id FROM bastion_tailscale b LEFT JOIN member_access a ON a.tailscale_id=b.id WHERE b.enabled=1 GROUP BY b.id ORDER BY count(a.member_id),b.id LIMIT 1)),
 account_id=COALESCE(account_id,(SELECT b.id FROM bastion_accounts b LEFT JOIN member_access a ON a.account_id=b.id WHERE b.enabled=1 GROUP BY b.id ORDER BY count(a.member_id),b.id LIMIT 1)) WHERE member_id=?`, id)
	return err
}
func (h *Handler) account(id string) (Account, error) {
	var a Account
	err := h.DB.SQL.QueryRow("SELECT id,username,home,uid,gid,host,port,enabled FROM bastion_accounts WHERE id=?", id).Scan(&a.ID, &a.Username, &a.Home, &a.UID, &a.GID, &a.Host, &a.Port, &a.Enabled)
	return a, err
}

type Access struct {
	MemberID    string `json:"member_id"`
	Username    string `json:"username"`
	TailscaleID string `json:"tailscale_id"`
	AccountID   string `json:"account_id"`
	Account     string `json:"account"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	InviteID    string `json:"invite_id"`
	InviteURL   string `json:"invite_url"`
	InviteState string `json:"invite_state"`
	AcceptedBy  string `json:"accepted_by"`
	KeyState    string `json:"key_state"`
	Error       string `json:"error"`
}

func (h *Handler) Access(id string) (Access, error) {
	var a Access
	err := h.DB.SQL.QueryRow(`SELECT a.member_id,m.username,COALESCE(a.tailscale_id,''),COALESCE(a.account_id,''),COALESCE(b.username,''),COALESCE(b.host,''),COALESCE(b.port,22),a.invite_id,a.invite_url,a.invite_state,a.accepted_by,a.key_state,a.error FROM member_access a JOIN members m ON m.id=a.member_id LEFT JOIN bastion_accounts b ON b.id=a.account_id WHERE a.member_id=?`, id).Scan(&a.MemberID, &a.Username, &a.TailscaleID, &a.AccountID, &a.Account, &a.Host, &a.Port, &a.InviteID, &a.InviteURL, &a.InviteState, &a.AcceptedBy, &a.KeyState, &a.Error)
	return a, err
}
func (h *Handler) Apply(ctx context.Context, id, key string, remove bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !remove {
		if err := h.DB.Transaction(func(tx *sql.Tx) error { return allocate(tx, id) }); err != nil {
			return err
		}
	}
	a, err := h.Access(id)
	if err != nil {
		return err
	}
	var problems []string
	if remove {
		if a.InviteState == "creating" || a.InviteState == "unknown" {
			problems = append(problems, "Tailscale 邀请结果待核对，需管理员关联邀请后再删除")
		} else if a.InviteState != "deleted" {
			if a.InviteID != "" {
				err = h.Tailscale.DeleteInvite(ctx, a.InviteID)
			}
			if err == nil {
				_, err = h.DB.SQL.Exec("UPDATE member_access SET invite_state='deleted',invite_url='' WHERE member_id=?", id)
			}
			if err != nil {
				problems = append(problems, err.Error())
			}
		}
	} else if a.InviteState == "pending" {
		if a.TailscaleID == "" {
			problems = append(problems, "暂无可分配的 Tailscale 节点")
		} else {
			before, e := h.Tailscale.Invites(ctx, a.TailscaleID)
			if e == nil {
				_, e = h.DB.SQL.Exec("UPDATE member_access SET invite_state='creating',invite_before=? WHERE member_id=?", httpapi.JSONText(before), id)
				if e == nil {
					invite, createErr := h.Tailscale.CreateInvite(ctx, a.TailscaleID)
					if createErr != nil {
						state := "unknown"
						if tailscale.CreationRejected(createErr) {
							state = "pending"
						}
						_, persistErr := h.DB.SQL.Exec("UPDATE member_access SET invite_state=? WHERE member_id=?", state, id)
						e = createErr
						if persistErr != nil {
							e = persistErr
						}
					} else {
						_, e = h.DB.SQL.Exec("UPDATE member_access SET invite_id=?,invite_url=?,invite_state='invited' WHERE member_id=?", invite.ID, invite.URL, id)
					}
				}
			}
			if e != nil {
				problems = append(problems, e.Error())
			}
		}
	}
	if remove && a.KeyState != "deleted" || !remove && a.KeyState != "ready" {
		if a.AccountID == "" {
			if remove {
				_, err = h.DB.SQL.Exec("UPDATE member_access SET key_state='deleted' WHERE member_id=?", id)
			} else {
				err = fmt.Errorf("暂无可分配的本机跳板账号")
			}
		} else {
			account, e := h.account(a.AccountID)
			err = e
			if err == nil {
				value := key
				if remove {
					value = ""
				}
				err = h.KeyEditor(account, id, value)
			}
			if err == nil {
				state := "ready"
				if remove {
					state = "deleted"
				}
				_, err = h.DB.SQL.Exec("UPDATE member_access SET key_state=? WHERE member_id=?", state, id)
			}
		}
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	_, err = h.DB.SQL.Exec("UPDATE member_access SET error=?,updated_at=? WHERE member_id=?", strings.Join(problems, "；"), platform.Now(), id)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "；"))
	}
	return nil
}
func (h *Handler) Refresh(ctx context.Context, id, resolve string, absent bool, actor string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, err := h.Access(id)
	if err != nil {
		return err
	}
	invites, err := h.Tailscale.Invites(ctx, a.TailscaleID)
	if err != nil {
		return err
	}
	if absent {
		if resolve != "" || (a.InviteState != "unknown" && a.InviteState != "creating") {
			return httpapi.NewError(409, "仅可确认结果未知且无邀请 ID 的分配")
		}
		var beforeRaw string
		if err = h.DB.SQL.QueryRow("SELECT invite_before FROM member_access WHERE member_id=?", id).Scan(&beforeRaw); err != nil {
			return err
		}
		var before []tailscale.Invite
		if err = json.Unmarshal([]byte(beforeRaw), &before); err != nil {
			return err
		}
		known := map[string]bool{}
		for _, v := range before {
			known[v.ID] = true
		}
		for _, v := range invites {
			if !known[v.ID] {
				return httpapi.NewError(409, "节点仍有新增邀请，请关联或在 Tailscale 控制台先核对并撤销")
			}
		}
		return h.DB.Transaction(func(tx *sql.Tx) error {
			if _, e := tx.Exec("UPDATE member_access SET invite_state='pending',error='',updated_at=? WHERE member_id=?", platform.Now(), id); e != nil {
				return e
			}
			return platform.Audit(tx, actor, "bastion.invite.absent", id)
		})
	}
	target := a.InviteID
	if resolve != "" {
		if a.InviteState != "unknown" && a.InviteState != "creating" {
			return httpapi.NewError(409, "仅可核对结果未知的邀请")
		}
		target = resolve
	}
	for _, v := range invites {
		if v.ID != target {
			continue
		}
		if !tailscale.ValidInvite(v) {
			return httpapi.NewError(409, "邀请必须是有效的单次分享")
		}
		if resolve != "" {
			var raw string
			if err = h.DB.SQL.QueryRow("SELECT invite_before FROM member_access WHERE member_id=?", id).Scan(&raw); err != nil {
				return err
			}
			var before []tailscale.Invite
			if err = json.Unmarshal([]byte(raw), &before); err != nil {
				return err
			}
			for _, old := range before {
				if old.ID == v.ID {
					return httpapi.NewError(409, "该邀请在本次分配前已存在")
				}
			}
		}
		state := "invited"
		if v.Accepted {
			state = "accepted"
		}
		return h.DB.Transaction(func(tx *sql.Tx) error {
			_, e := tx.Exec("UPDATE member_access SET invite_id=?,invite_url=?,invite_state=?,accepted_by=?,error='',updated_at=? WHERE member_id=?", v.ID, v.URL, state, v.AcceptedBy.LoginName, platform.Now(), id)
			if platform.IsConstraint(e) {
				return httpapi.NewError(409, "该邀请已关联其他使用者")
			}
			if e != nil {
				return e
			}
			return platform.Audit(tx, actor, "bastion.invite.sync", id)
		})
	}
	return httpapi.NewError(409, "未找到对应邀请；请在 Tailscale 控制台核对，不自动重新分享")
}
func read(w http.ResponseWriter, r *http.Request, out any) error {
	v, e := httpapi.RequestBody(w, r)
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(v)
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return httpapi.NewError(400, "请求字段无效")
	}
	return nil
}
func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	if u.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if tailscale.IsRoute(r.URL.Path) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Method == "PUT" {
			var count int
			if err := h.DB.SQL.QueryRow("SELECT count(*) FROM bastion_tailscale").Scan(&count); err != nil {
				return 0, nil, err
			}
			if count > 0 {
				// Key rotation is allowed, changing networks or clearing credentials is not.
				fields, e := httpapi.RequestBody(w, r)
				if e != nil {
					return 0, nil, e
				}
				var old string
				if e = h.DB.SQL.QueryRow("SELECT tailnet FROM tailscale_settings WHERE id=1").Scan(&old); e != nil {
					return 0, nil, e
				}
				if httpapi.FieldString(fields, "tailnet") != old || string(fields["clear_api_token"]) == "true" {
					return 0, nil, httpapi.NewError(409, "请先清理分享和节点池，再切换 Tailnet 或清除凭据")
				}
				raw, _ := json.Marshal(fields)
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
			}
		}
		return h.Tailscale.Dispatch(w, r, u)
	}
	if r.Method == "GET" && r.URL.Path == "/api/bastion/resources" {
		t, e := platform.Rows(h.DB.SQL, "SELECT b.*,count(a.member_id) AS member_count FROM bastion_tailscale b LEFT JOIN member_access a ON a.tailscale_id=b.id GROUP BY b.id ORDER BY b.name")
		if e != nil {
			return 0, nil, e
		}
		accounts, e := platform.Rows(h.DB.SQL, "SELECT b.*,count(a.member_id) AS member_count FROM bastion_accounts b LEFT JOIN member_access a ON a.account_id=b.id GROUP BY b.id ORDER BY b.username")
		if e != nil {
			return 0, nil, e
		}
		assignments, e := platform.Rows(h.DB.SQL, `SELECT a.*,m.username,m.status AS member_status,COALESCE(b.username,'') AS account FROM member_access a JOIN members m ON m.id=a.member_id LEFT JOIN bastion_accounts b ON b.id=a.account_id ORDER BY m.created_at DESC`)
		for _, a := range assignments {
			delete(a, "invite_before")
		}
		return 200, map[string]any{"tailscale": t, "accounts": accounts, "assignments": assignments}, e
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/bastion/"), "/")
	if len(parts) == 3 && parts[0] == "members" && (parts[2] == "refresh" || parts[2] == "resolve-invite") && r.Method == "POST" {
		var req struct {
			InviteID      string `json:"invite_id"`
			ConfirmAbsent bool   `json:"confirm_absent"`
		}
		if e := read(w, r, &req); e != nil {
			return 0, nil, e
		}
		if parts[2] == "resolve-invite" && req.InviteID == "" && !req.ConfirmAbsent {
			return 0, nil, httpapi.NewError(400, "需指定邀请 ID")
		}
		e := h.Refresh(r.Context(), parts[1], req.InviteID, req.ConfirmAbsent, u.Username)
		return 200, map[string]bool{"ok": e == nil}, e
	}
	if len(parts) == 3 && parts[0] == "tailscale" && parts[2] == "invites" && r.Method == "GET" {
		v, e := h.Tailscale.Invites(r.Context(), parts[1])
		return 200, map[string]any{"invites": v}, e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(parts) == 1 && parts[0] == "accounts" && r.Method == "POST" {
		var req struct {
			Username string `json:"username"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
		}
		if e := read(w, r, &req); e != nil {
			return 0, nil, e
		}
		if !validHost(req.Host) || req.Port < 1 || req.Port > 65535 {
			return 0, nil, httpapi.NewError(400, "请提供 SSH 主机和有效端口")
		}
		a, e := lookupAccount(req.Username)
		if e != nil {
			return 0, nil, httpapi.NewError(400, e.Error())
		}
		a.ID = platform.RandomHex(16)
		a.Host = req.Host
		a.Port = req.Port
		a.Enabled = true
		e = h.DB.Transaction(func(tx *sql.Tx) error {
			_, e := tx.Exec("INSERT INTO bastion_accounts VALUES(?,?,?,?,?,?,?,1)", a.ID, a.Username, a.Home, a.UID, a.GID, a.Host, a.Port)
			if e != nil {
				return e
			}
			return platform.Audit(tx, u.Username, "bastion.account.add", a.Username)
		})
		if platform.IsConstraint(e) {
			e = httpapi.NewError(409, "该本机账号已添加")
		}
		return 201, a, e
	}
	if len(parts) == 2 && (parts[0] == "tailscale" || parts[0] == "accounts") {
		table := "bastion_" + parts[0]
		column := "account_id"
		if parts[0] == "tailscale" {
			column = "tailscale_id"
		}
		if r.Method == "DELETE" {
			e := h.DB.Transaction(func(tx *sql.Tx) error {
				var count int
				if e := tx.QueryRow("SELECT count(*) FROM member_access WHERE "+column+"=?", parts[1]).Scan(&count); e != nil {
					return e
				}
				if count > 0 {
					return httpapi.NewError(409, "该资源仍有关联使用者，请先回收分配")
				}
				if _, e := tx.Exec("DELETE FROM "+table+" WHERE id=?", parts[1]); e != nil {
					return e
				}
				return platform.Audit(tx, u.Username, "bastion.resource.remove", parts[1])
			})
			return 200, map[string]bool{"ok": e == nil}, e
		}
		if r.Method == "PUT" {
			var req struct {
				Enabled *bool `json:"enabled"`
			}
			if e := read(w, r, &req); e != nil {
				return 0, nil, e
			}
			if req.Enabled == nil {
				return 0, nil, httpapi.NewError(400, "需提供 enabled")
			}
			name := ""
			if parts[0] == "tailscale" && !*req.Enabled {
				if e := h.DB.SQL.QueryRow("SELECT name FROM bastion_tailscale WHERE id=?", parts[1]).Scan(&name); e != nil {
					if e == sql.ErrNoRows {
						return 0, nil, httpapi.NewError(404, "节点池资源不存在")
					}
					return 0, nil, e
				}
			}
			if parts[0] == "tailscale" && *req.Enabled {
				devices, e := h.Tailscale.Devices(r.Context())
				if e != nil {
					return 0, nil, e
				}
				for _, d := range devices {
					if d.NodeID == parts[1] && !d.IsExternal && d.Authorized {
						name = d.Hostname
						if name == "" {
							name = d.Name
						}
					}
				}
				if name == "" {
					return 0, nil, httpapi.NewError(400, "请选择当前网络中已授权的自有节点")
				}
			}
			e := h.DB.Transaction(func(tx *sql.Tx) error {
				var e error
				if parts[0] == "tailscale" {
					_, e = tx.Exec("INSERT INTO bastion_tailscale VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled,name=CASE WHEN excluded.name='' THEN name ELSE excluded.name END", parts[1], name, *req.Enabled)
				} else {
					var result sql.Result
					result, e = tx.Exec("UPDATE bastion_accounts SET enabled=? WHERE id=?", *req.Enabled, parts[1])
					if e == nil {
						n, _ := result.RowsAffected()
						if n != 1 {
							return httpapi.NewError(404, "账号不存在")
						}
					}
				}
				if e != nil {
					return e
				}
				return platform.Audit(tx, u.Username, "bastion.resource.update", parts[1])
			})
			return 200, map[string]bool{"ok": e == nil}, e
		}
	}
	return 0, nil, httpapi.NewError(404, "接口不存在")
}
