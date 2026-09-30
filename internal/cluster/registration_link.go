package cluster

import (
	"net/http"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/registry"
)

func (h *Control) registrationLink(w http.ResponseWriter, r *http.Request, n Node) (int, any, error) {
	if n.Kind != "registry" {
		return 0, nil, httpapi.NewError(400, "只有 registry 节点支持生成注册链接")
	}
	var input struct {
		InvitationID string `json:"invitation_id"`
	}
	if err := httpapi.DecodeBody(w, r, &input); err != nil {
		return 0, nil, err
	}
	if !identifier.MatchString(input.InvitationID) {
		return 0, nil, httpapi.NewError(400, "请选择有效的邀请码")
	}
	info, err := h.probe(r.Context(), n)
	if err != nil {
		return 0, nil, err
	}
	if info.ID != n.ID {
		return 0, nil, httpapi.NewError(409, "registry 实例身份已变化，请重新添加节点")
	}
	pass := strings.TrimSuffix(strings.TrimPrefix(info.RegistrationPath, "/registry/"), "/")
	if !registry.ValidPass(pass) || info.RegistrationPath != "/registry/"+pass+"/" {
		return 0, nil, httpapi.NewError(502, "registry 返回的注册入口格式无效")
	}
	code, err := (&members.Store{Database: h.DB}).InvitationCode(input.InvitationID)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]string{"url": n.URL + info.RegistrationPath + code}, nil
}
