package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/registry"
)

// RegistryDispatch is a narrow capability boundary: no admin or arbitrary proxy API.
func (h *Control) RegistryDispatch(ctx context.Context, req registry.Request) (any, error) {
	switch req.Action {
	case "validate":
		store := &members.Store{Database: h.DB}
		if err := store.CheckInvitation(req.Invitation); err != nil {
			return nil, err
		}
		return store.Schema()
	case "register":
		var registration members.Registration
		decoder := json.NewDecoder(bytes.NewReader(req.Body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&registration); err != nil {
			return nil, httpapi.NewError(400, "注册字段无效")
		}
		if registration.InvitationCode != req.Invitation {
			return nil, httpapi.NewError(400, "邀请码不匹配")
		}
		m, err := h.Members.RegisterRegistry(registration, req.Token)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": m.ID, "username": m.Username}, nil
	case "resources", "retry":
		r, _ := http.NewRequestWithContext(ctx, "POST", "http://control/api/members/me/"+req.Action, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+req.Token)
		r.Header.Set("Content-Type", "application/json")
		id, err := h.memberID(r)
		if err != nil {
			return nil, err
		}
		if req.Action == "retry" {
			_, value, err := h.dispatchMemberResource(nil, r, id, "retry", false, id)
			return value, err
		}
		return h.memberResources(id)
	default:
		return nil, httpapi.NewError(404, "registry 操作不存在")
	}
}
