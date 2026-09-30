package platform

import (
	"database/sql"
	"net/netip"
	"strings"

	"project-alpha/internal/httpapi"
)

// ControlSettings defines the internal control address allowed by the HTTP server.
type ControlSettings struct {
	Revision   int    `json:"revision"`
	InternalIP string `json:"internal_ip"`
}

func InternalIP(value string) (string, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsLoopback() {
		return "", httpapi.NewError(400, "内网 IP 必填，需为有效的 IPv4 或 IPv6 地址，不能包含端口或使用回环地址")
	}
	return address.Unmap().String(), nil
}

func (v *ControlSettings) validate() error {
	var err error
	if v.InternalIP, err = InternalIP(v.InternalIP); err != nil {
		return err
	}
	return nil
}

func (d *Database) ControlSettings() (ControlSettings, error) {
	var v ControlSettings
	err := d.SQL.QueryRow("SELECT revision,internal_ip FROM control_settings WHERE id=1").Scan(&v.Revision, &v.InternalIP)
	if err == sql.ErrNoRows {
		return v, httpapi.NewError(503, "请先初始化管理员并填写总控内网 IP")
	}
	return v, err
}

func (d *Database) UpdateControlSettings(v ControlSettings, actor string) (ControlSettings, error) {
	if err := v.validate(); err != nil {
		return v, err
	}
	err := d.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE control_settings SET revision=revision+1,internal_ip=? WHERE id=1 AND revision=?", v.InternalIP, v.Revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return httpapi.NewError(409, "总控配置已更新，请重新载入后保存")
		}
		return Audit(tx, actor, "control.settings", v.InternalIP)
	})
	v.Revision++
	return v, err
}
