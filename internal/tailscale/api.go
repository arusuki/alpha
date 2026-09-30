package tailscale

import (
	"net/http"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Handler struct {
	db     *platform.Database
	client *http.Client
}

func NewHandler(db *platform.Database) *Handler { return &Handler{db: db, client: newClient()} }
func IsRoute(path string) bool {
	return path == "/api/tailscale" || strings.HasPrefix(path, "/api/tailscale/")
}

func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "此操作需要管理员权限")
	}
	if r.URL.Path == "/api/tailscale/settings" {
		switch r.Method {
		case "GET":
			s, _, err := h.settings()
			return 200, s, err
		case "PUT":
			fields, err := httpapi.RequestBody(w, r)
			if err != nil {
				return 0, nil, err
			}
			s, err := h.saveSettings(fields, user.Username)
			return 200, s, err
		}
	}
	isTest := r.URL.Path == "/api/tailscale/test" && r.Method == "POST"
	if !isTest && !(r.URL.Path == "/api/tailscale/devices" && r.Method == "GET") {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	if isTest {
		fields, err := httpapi.RequestBody(w, r)
		if err != nil {
			return 0, nil, err
		}
		if len(fields) != 0 {
			return 0, nil, httpapi.NewError(400, "测试连接使用已保存配置，请提交空对象")
		}
	}
	s, ciphertext, err := h.settings()
	if err != nil {
		return 0, nil, err
	}
	if !s.HasAPIToken {
		return 0, nil, httpapi.NewError(409, "请先保存 Tailscale API Token")
	}
	token, err := h.decrypt(ciphertext)
	if err != nil {
		return 0, nil, err
	}
	devices, err := h.listDevices(r.Context(), s, token)
	if err != nil {
		return 0, nil, err
	}
	if isTest {
		return 200, map[string]any{"device_count": len(devices), "checked_at": platform.Now(), "revision": s.Revision, "tailnet": s.Tailnet}, nil
	}
	return 200, map[string]any{"devices": devices, "fetched_at": platform.Now(), "revision": s.Revision, "tailnet": s.Tailnet}, nil
}
