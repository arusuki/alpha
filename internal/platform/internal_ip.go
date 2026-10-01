package platform

import (
	"net/netip"
	"strings"

	"project-alpha/internal/httpapi"
)

func InternalIP(value string) (string, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsLoopback() {
		return "", httpapi.NewError(400, "内网 IP 必填，需为有效的 IPv4 或 IPv6 地址，不能包含端口或使用回环地址")
	}
	return address.Unmap().String(), nil
}
