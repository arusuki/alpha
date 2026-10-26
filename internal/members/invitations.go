package members

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"html/template"
	"mime"
	"net/http"
	"regexp"
	"strconv"

	"project-alpha/internal/httpapi"
)

// Invitation management is a server-rendered admin view with form actions,
// not part of the JSON API. The platform applies session, admin and CSRF guards.
const invitationPage = "/admin/member-invitations"

//go:embed invitations.html
var invitationHTML string

var invitationTemplate = template.Must(template.New("invitations").Parse(invitationHTML))
var invitationID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (h *Handler) invitationView(w http.ResponseWriter, r *http.Request, actor string) (int, any, error) {
	code := ""
	switch {
	case r.Method == "GET" && r.URL.Path == invitationPage:
	case r.Method == "GET" && r.URL.Path == invitationPage+"/select":
	case r.Method == "POST" && (r.URL.Path == invitationPage+"/create" || r.URL.Path == invitationPage+"/update" || r.URL.Path == invitationPage+"/revoke" || r.URL.Path == invitationPage+"/delete"):
		typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || typ != "application/x-www-form-urlencoded" {
			return 0, nil, httpapi.NewError(415, "请通过管理员页面提交邀请码表单")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if err = r.ParseForm(); err != nil {
			return 0, nil, httpapi.NewError(400, "邀请码表单无效或过大")
		}
		create := r.URL.Path == invitationPage+"/create"
		update := r.URL.Path == invitationPage+"/update"
		for key, values := range r.PostForm {
			if len(values) != 1 || (create && key != "label" && key != "quota" && key != "node_permissions") || (update && key != "id" && key != "label") || (!create && !update && key != "id") {
				return 0, nil, httpapi.NewError(400, "邀请码表单字段无效")
			}
		}
		if update && !r.PostForm.Has("label") {
			return 0, nil, httpapi.NewError(400, "邀请码表单缺少备注字段")
		}
		if create {
			quota, err := strconv.Atoi(r.PostForm.Get("quota"))
			if err != nil {
				return 0, nil, httpapi.NewError(400, "可注册人数必须是整数")
			}
			var permissions NodePermissions
			if err := json.Unmarshal([]byte(r.PostForm.Get("node_permissions")), &permissions); err != nil {
				return 0, nil, httpapi.NewError(400, "请指定邀请码的节点权限")
			}
			invitation, err := h.store.CreateInvitation(r.PostForm.Get("label"), quota, actor, permissions)
			if err != nil {
				return 0, nil, err
			}
			code = invitation.Code
		} else {
			id := r.PostForm.Get("id")
			if !invitationID.MatchString(id) {
				return 0, nil, httpapi.NewError(400, "邀请码标识无效")
			}
			var err error
			if update {
				err = h.store.UpdateInvitationLabel(id, r.PostForm.Get("label"), actor)
			} else if r.URL.Path == invitationPage+"/delete" {
				err = h.store.DeleteInvitation(id, actor)
			} else {
				err = h.store.RevokeInvitation(id, actor)
			}
			if err != nil {
				return 0, nil, err
			}
		}
	default:
		return 0, nil, httpapi.NewError(404, "页面不存在")
	}
	rows, err := h.store.Invitations()
	if err != nil {
		return 0, nil, err
	}
	for i := range rows {
		for _, id := range rows[i].NodePermissions.NodeIDs {
			var name string
			err := h.store.SQL.QueryRow("SELECT name FROM cluster_nodes WHERE id=? AND kind='worker'", id).Scan(&name)
			if err == sql.ErrNoRows {
				name = "已移除节点"
			} else if err != nil {
				return 0, nil, err
			}
			rows[i].NodeNames = append(rows[i].NodeNames, name)
		}
	}
	var body bytes.Buffer
	view := "invitations"
	if r.URL.Path == invitationPage+"/select" {
		view = "invitation-options"
	}
	if err = invitationTemplate.ExecuteTemplate(&body, view, struct {
		Invitations []Invitation
		Code        string
	}{rows, code}); err != nil {
		return 0, nil, err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body.Bytes())
	return 0, nil, err
}
