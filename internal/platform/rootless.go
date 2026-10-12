package platform

import (
	"database/sql"
	"fmt"

	"project-alpha/internal/containerorigin"
)

func RootlessControlSocket(db *sql.DB) (string, error) {
	// Storage can run as an independent module without node-service management.
	var installed bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='node_service_settings')").Scan(&installed); err != nil {
		return "", err
	}
	if !installed {
		return containerorigin.DefaultSocket, nil
	}
	var socket string
	err := db.QueryRow("SELECT json_extract(config,'$.control_socket') FROM node_service_settings WHERE name='rootless-docker'").Scan(&socket)
	if err != nil && err != sql.ErrNoRows {
		return "", fmt.Errorf("读取 rootless Docker 配置: %w", err)
	}
	if socket == "" {
		socket = containerorigin.DefaultSocket
	}
	return socket, nil
}
