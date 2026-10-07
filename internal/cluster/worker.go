// Package cluster connects API-only workers to the central management server.
package cluster

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/updates"
)

const Protocol = 7

var identifier = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Container struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	State      string `json:"state"`
	Managed    bool   `json:"managed"`
	ObservedAt string `json:"observed_at,omitempty"`
}
type Inventory struct {
	Host       string      `json:"host"`
	Containers []Container `json:"containers"`
	SnapshotID string      `json:"snapshot_id,omitempty"`
	ObservedAt string      `json:"observed_at,omitempty"`
	Active     any         `json:"active"`
}
type Info struct {
	ManagementProtocol int    `json:"management_protocol"`
	Version            string `json:"version"`
	Protocol           int    `json:"protocol"`
	ID                 string `json:"id"`
	Mode               string `json:"mode"`
	RegistrationPath   string `json:"registration_path,omitempty"`
}
type Worker struct {
	Updates   *updates.Manager
	ID, Token string
	Module    platform.Module
	Inventory func() (Inventory, error)
	Tools     func(http.ResponseWriter, *http.Request, platform.User) (int, any, error)
}

func ValidToken(token string) bool {
	if len(token) < 32 || len(token) > 256 {
		return false
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	var api *httpapi.Error
	if errors.As(err, &api) {
		httpapi.WriteJSON(w, api.Status, map[string]string{"error": api.Message})
		return
	}
	log.Printf("cluster: %v", err)
	httpapi.WriteJSON(w, 500, map[string]string{"error": "集群服务内部错误，请检查日志"})
}

func operational(path string) bool {
	for _, prefix := range []string{"/api/state", "/api/settings", "/api/jobs", "/api/owners", "/api/containers", "/api/process", "/api/gpu"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return path == "/api/snapshot"
}

func (h *Worker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() {
		if p := recover(); p != nil {
			log.Printf("worker panic: %v", p)
			writeError(w, httpapi.NewError(500, "节点内部错误"))
		}
	}()
	if !ValidToken(h.Token) || !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.Token)) {
		writeError(w, httpapi.NewError(401, "请通过总控访问此节点"))
		return
	}
	if r.URL.Path == "/api/worker/info" && r.Method == "GET" {
		httpapi.WriteJSON(w, 200, Info{Protocol: Protocol, ID: h.ID, Mode: "worker", ManagementProtocol: updates.Protocol, Version: buildinfo.Version})
		return
	}
	if r.Header.Get("X-Alpha-Node") != h.ID {
		writeError(w, httpapi.NewError(409, "节点身份不匹配，请在总控重新添加节点"))
		return
	}
	var user platform.User
	if json.Unmarshal([]byte(r.Header.Get("X-Alpha-User")), &user) != nil || !identifier.MatchString(user.ID) || user.Username == "" || len(user.Username) > 48 || (user.Role != "admin" && user.Role != "viewer") {
		writeError(w, httpapi.NewError(401, "总控用户身份无效"))
		return
	}
	if r.Method != "GET" && user.Role != "admin" {
		writeError(w, httpapi.NewError(403, "此操作需要管理员权限"))
		return
	}
	if strings.HasPrefix(r.URL.Path, updates.Path+"/") && h.Updates != nil {
		if user.Role != "admin" && r.URL.Path != updates.Path+"/health" {
			writeError(w, httpapi.NewError(403, "更新设置需要管理员权限"))
			return
		}
		status, value, err := h.Updates.Dispatch(w, r)
		if err != nil {
			writeError(w, err)
		} else {
			httpapi.WriteJSON(w, status, value)
		}
		return
	}
	if r.URL.Path == "/api/worker/inventory" && r.Method == "GET" {
		value, err := h.Inventory()
		if err != nil {
			writeError(w, err)
			return
		}
		httpapi.WriteJSON(w, 200, value)
		return
	}
	if r.URL.Path == "/api/worker/records" && r.Method == "POST" && h.Tools != nil {
		status, value, err := h.Tools(w, platform.WithRemoteUser(r, user), user)
		if err != nil {
			writeError(w, err)
		} else if status != 0 {
			httpapi.WriteJSON(w, status, value)
		}
		return
	}
	if !operational(r.URL.Path) {
		writeError(w, httpapi.NewError(404, "node 只提供节点操作 API，用户管理请使用总控"))
		return
	}
	status, value, err := h.Module.Dispatch(w, platform.WithRemoteUser(r, user), user)
	if err != nil {
		writeError(w, err)
		return
	}
	if status != 0 {
		httpapi.WriteJSON(w, status, value)
	}
}
