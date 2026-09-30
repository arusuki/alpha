package platform

import (
	"database/sql"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
)

// ControlSettings defines the member-facing address of control, including when
// its web interface is served through a reverse proxy.
type ControlSettings struct {
	Revision   int    `json:"revision"`
	InternalIP string `json:"internal_ip"`
	WebScheme  string `json:"web_scheme"`
	WebPort    int    `json:"web_port"`
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
	if v.WebScheme != "http" && v.WebScheme != "https" {
		return httpapi.NewError(400, "总控网页协议需为 HTTP 或 HTTPS")
	}
	if v.WebPort < 1 || v.WebPort > 65535 {
		return httpapi.NewError(400, "总控网页端口需为 1–65535")
	}
	return nil
}

func (v ControlSettings) StatusURL(username string) string {
	host := net.JoinHostPort(v.InternalIP, strconv.Itoa(v.WebPort))
	if v.WebScheme == "http" && v.WebPort == 80 || v.WebScheme == "https" && v.WebPort == 443 {
		host = v.InternalIP
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return (&url.URL{Scheme: v.WebScheme, Host: host, Path: "/status/" + username}).String()
}

func (d *Database) ControlSettings() (ControlSettings, error) {
	var v ControlSettings
	err := d.SQL.QueryRow("SELECT revision,internal_ip,web_scheme,web_port FROM control_settings WHERE id=1").Scan(&v.Revision, &v.InternalIP, &v.WebScheme, &v.WebPort)
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
		result, err := tx.Exec("UPDATE control_settings SET revision=revision+1,internal_ip=?,web_scheme=?,web_port=? WHERE id=1 AND revision=?", v.InternalIP, v.WebScheme, v.WebPort, v.Revision)
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
		return Audit(tx, actor, "control.settings", v.InternalIP+" / "+v.WebScheme+" / "+strconv.Itoa(v.WebPort))
	})
	v.Revision++
	return v, err
}
