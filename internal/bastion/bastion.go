// Package bastion manages shared access through remote share nodes.
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
	"sync/atomic"

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
	DB            *platform.Database
	Tailscale     Network
	mu            sync.Mutex
	KeyEditor     func(context.Context, string, string) error
	RemoteCommand func(context.Context, ShareNode, commandRequest) (commandReply, error)
	sshConfig     atomic.Pointer[sshSettings]
	sshConfigErr  error
}

func NewHandler(db *platform.Database) *Handler {
	h := &Handler{DB: db, Tailscale: tailscale.NewHandler(db)}
	cfg, err := h.sshSettings()
	h.sshConfigErr = err
	if err == nil {
		h.sshConfig.Store(&cfg)
	}
	h.RemoteCommand = h.sshCommand
	h.KeyEditor = h.editMemberKey
	return h
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
	_, err := tx.Exec(`UPDATE member_access SET tailscale_id=COALESCE(tailscale_id,(SELECT b.id FROM bastion_tailscale b LEFT JOIN member_access a ON a.tailscale_id=b.id WHERE b.enabled=1 GROUP BY b.id ORDER BY count(a.member_id),b.id LIMIT 1)) WHERE member_id=?`, id)
	return err
}

type Access struct {
	MemberID     string `json:"member_id"`
	Username     string `json:"username"`
	TailscaleID  string `json:"tailscale_id"`
	InviteID     string `json:"invite_id"`
	InviteURL    string `json:"invite_url"`
	InviteState  string `json:"invite_state"`
	AcceptedBy   string `json:"accepted_by"`
	KeyState     string `json:"key_state"`
	Error        string `json:"error"`
	ShareHost    string `json:"share_host"`
	ShareSSHPort int    `json:"share_ssh_port"`
	StatusPort   int    `json:"status_port"`
}

func (h *Handler) Access(id string) (Access, error) {
	var a Access
	err := h.DB.SQL.QueryRow(`SELECT a.member_id,m.username,COALESCE(a.tailscale_id,''),a.invite_id,a.invite_url,a.invite_state,a.accepted_by,a.key_state,a.error,COALESCE(b.ssh_host,''),COALESCE(b.ssh_port,0),COALESCE(b.status_port,0) FROM member_access a JOIN members m ON m.id=a.member_id LEFT JOIN bastion_tailscale b ON b.id=a.tailscale_id WHERE a.member_id=?`, id).Scan(&a.MemberID, &a.Username, &a.TailscaleID, &a.InviteID, &a.InviteURL, &a.InviteState, &a.AcceptedBy, &a.KeyState, &a.Error, &a.ShareHost, &a.ShareSSHPort, &a.StatusPort)
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
		value := key
		if remove {
			value = ""
		}
		err = h.KeyEditor(ctx, id, value)
		if err == nil {
			state := "ready"
			if remove {
				state = "deleted"
			}
			_, err = h.DB.SQL.Exec("UPDATE member_access SET key_state=? WHERE member_id=?", state, id)
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
func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	if u.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if strings.HasPrefix(r.URL.Path, "/api/bastion/ssh") {
		return h.dispatchSSH(w, r, u)
	}
	if r.Method == "GET" && r.URL.Path == "/api/bastion/keys" {
		keys, problem, err := h.keyPool(r.Context())
		return 200, map[string]any{"keys": keys, "error": problem}, err
	}
	if r.Method == "POST" && r.URL.Path == "/api/bastion/keys/sync" {
		if err := httpapi.DecodeBody(w, r, &struct{}{}); err != nil {
			return 0, nil, err
		}
		err := h.SyncKeys(r.Context())
		if err == nil {
			err = platform.Audit(h.DB.SQL, u.Username, "bastion.key.sync", JumpUser)
		}
		return 200, map[string]bool{"ok": err == nil}, err
	}
	if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/bastion/keys/") {
		if err := httpapi.DecodeBody(w, r, &struct{}{}); err != nil {
			return 0, nil, err
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/bastion/keys/"), "/")
		if len(parts) != 2 {
			return 0, nil, httpapi.NewError(400, "需指定 share node 和公钥条目")
		}
		err := h.cleanFreeKey(r.Context(), parts[0], parts[1], u.Username)
		return 200, map[string]bool{"ok": err == nil}, err
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
		assignments, e := platform.Rows(h.DB.SQL, `SELECT a.*,m.username,m.status AS member_status FROM member_access a JOIN members m ON m.id=a.member_id ORDER BY m.created_at DESC`)
		for _, a := range assignments {
			delete(a, "invite_before")
		}
		keys, problem, poolErr := h.keyPool(r.Context())
		if poolErr != nil {
			return 0, nil, poolErr
		}
		return 200, map[string]any{"tailscale": t, "assignments": assignments, "key_pool": map[string]any{"keys": keys, "error": problem}}, e
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/bastion/"), "/")
	if len(parts) == 3 && parts[0] == "members" && (parts[2] == "refresh" || parts[2] == "resolve-invite") && r.Method == "POST" {
		var req struct {
			InviteID      string `json:"invite_id"`
			ConfirmAbsent bool   `json:"confirm_absent"`
		}
		if e := httpapi.DecodeBody(w, r, &req); e != nil {
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
	if len(parts) == 2 && parts[0] == "tailscale" {
		if r.Method == "DELETE" {
			e := h.DB.Transaction(func(tx *sql.Tx) error {
				var count int
				if e := tx.QueryRow("SELECT count(*) FROM member_access WHERE tailscale_id=?", parts[1]).Scan(&count); e != nil {
					return e
				}
				if count > 0 {
					return httpapi.NewError(409, "该资源仍有关联使用者，请先回收分配")
				}
				if _, e := tx.Exec("DELETE FROM bastion_tailscale WHERE id=?", parts[1]); e != nil {
					return e
				}
				return platform.Audit(tx, u.Username, "bastion.resource.remove", parts[1])
			})
			return 200, map[string]bool{"ok": e == nil}, e
		}
		if r.Method == "PUT" {
			var req shareUpdate
			if err := httpapi.DecodeBody(w, r, &req); err != nil {
				return 0, nil, err
			}
			err := h.updateShare(r.Context(), parts[1], req, u.Username)
			return 200, map[string]bool{"ok": err == nil}, err
		}
	}
	return 0, nil, httpapi.NewError(404, "接口不存在")
}
