package bastion

import (
	"context"
	"database/sql"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type ShareNode struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	SSHHost    string `json:"ssh_host"`
	SSHPort    int    `json:"ssh_port"`
	StatusPort int    `json:"status_port"`
	ControlURL string `json:"control_url"`
}
type shareUpdate struct {
	Enabled    *bool   `json:"enabled"`
	SSHHost    *string `json:"ssh_host"`
	SSHPort    *int    `json:"ssh_port"`
	StatusPort *int    `json:"status_port"`
}

func (s ShareNode) validate() error {
	host, err := platform.InternalIP(s.SSHHost)
	if err != nil {
		return err
	}
	if host != s.SSHHost || s.SSHPort < 1 || s.SSHPort > 65535 || s.StatusPort < 1024 || s.StatusPort > 65535 || s.SSHPort == s.StatusPort {
		return httpapi.NewError(400, "分享节点需使用有效 IP、SSH 端口及不同的 1024–65535 入口端口")
	}
	return nil
}
func (h *Handler) share(id string) (ShareNode, error) {
	var s ShareNode
	err := h.DB.SQL.QueryRow("SELECT id,name,enabled,ssh_host,ssh_port,status_port,control_url FROM bastion_tailscale WHERE id=?", id).Scan(&s.ID, &s.Name, &s.Enabled, &s.SSHHost, &s.SSHPort, &s.StatusPort, &s.ControlURL)
	return s, err
}
func (h *Handler) shares() ([]ShareNode, error) {
	rows, err := h.DB.SQL.Query("SELECT id,name,enabled,ssh_host,ssh_port,status_port,control_url FROM bastion_tailscale ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareNode{}
	for rows.Next() {
		var s ShareNode
		if err = rows.Scan(&s.ID, &s.Name, &s.Enabled, &s.SSHHost, &s.SSHPort, &s.StatusPort, &s.ControlURL); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *Handler) updateShare(ctx context.Context, id string, req shareUpdate, actor string) error {
	if req.Enabled == nil {
		return httpapi.NewError(400, "需提供 enabled")
	}
	saved, e := h.share(id)
	exists := e == nil
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	candidate := saved
	if !exists {
		candidate = ShareNode{ID: id, SSHPort: 22, StatusPort: 8765}
	}
	candidate.Enabled = *req.Enabled
	if req.SSHHost != nil {
		candidate.SSHHost = *req.SSHHost
	}
	if req.SSHPort != nil {
		candidate.SSHPort = *req.SSHPort
	}
	if req.StatusPort != nil {
		candidate.StatusPort = *req.StatusPort
	}
	if !candidate.Enabled && !exists {
		return httpapi.NewError(404, "节点池资源不存在")
	}
	checkNode := candidate.Enabled || req.SSHHost != nil || req.SSHPort != nil || req.StatusPort != nil
	if checkNode {
		devices, e := h.Tailscale.Devices(ctx)
		if e != nil {
			return e
		}
		found := false
		for _, d := range devices {
			if d.NodeID != candidate.ID || d.IsExternal || !d.Authorized {
				continue
			}
			candidate.Name = d.Hostname
			if candidate.Name == "" {
				candidate.Name = d.Name
			}
			for _, address := range d.Addresses {
				ip, e := platform.InternalIP(address)
				if e != nil {
					continue
				}
				if candidate.SSHHost == "" {
					candidate.SSHHost = ip
				}
				if candidate.SSHHost == ip {
					found = true
				}
			}
		}
		if !found {
			return httpapi.NewError(400, "请选择当前网络已授权的自有节点及其 Tailscale IP")
		}
	}
	if e := candidate.validate(); e != nil {
		return e
	}
	if checkNode {
		candidate.ControlURL = ""
		reply, e := h.RemoteCommand(ctx, candidate, commandRequest{Operation: "inspect"})
		if e != nil {
			return e
		}
		candidate.ControlURL = reply.ControlURL
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		if exists && (saved.SSHHost != candidate.SSHHost || saved.SSHPort != candidate.SSHPort || saved.StatusPort != candidate.StatusPort) {
			var count int
			if e := tx.QueryRow("SELECT count(*) FROM member_access WHERE tailscale_id=?", candidate.ID).Scan(&count); e != nil {
				return e
			}
			if count > 0 {
				return httpapi.NewError(409, "分享节点仍有成员引用，不能更改 SSH 或入口地址")
			}
		}
		_, e := tx.Exec("INSERT INTO bastion_tailscale(id,name,enabled,ssh_host,ssh_port,status_port,control_url) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,enabled=excluded.enabled,ssh_host=excluded.ssh_host,ssh_port=excluded.ssh_port,status_port=excluded.status_port,control_url=excluded.control_url", candidate.ID, candidate.Name, candidate.Enabled, candidate.SSHHost, candidate.SSHPort, candidate.StatusPort, candidate.ControlURL)
		if e != nil {
			if platform.IsConstraint(e) {
				return httpapi.NewError(409, "分享节点的入口地址和端口已在池中使用")
			}
			return e
		}
		return platform.Audit(tx, actor, "bastion.resource.update", candidate.ID)
	})
}
