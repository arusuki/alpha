package members

import (
	"bytes"
	_ "embed"
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
	case r.Method == "POST" && (r.URL.Path == invitationPage+"/create" || r.URL.Path == invitationPage+"/revoke"):
		typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || typ != "application/x-www-form-urlencoded" {
			return 0, nil, httpapi.NewError(415, "请通过管理员页面提交邀请码表单")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err = r.ParseForm(); err != nil {
			return 0, nil, httpapi.NewError(400, "邀请码表单无效或过大")
		}
		create := r.URL.Path == invitationPage+"/create"
		for key, values := range r.PostForm {
			if len(values) != 1 || (create && key != "label" && key != "quota") || (!create && key != "id") {
				return 0, nil, httpapi.NewError(400, "邀请码表单字段无效")
			}
		}
		if create {
			quota, err := strconv.Atoi(r.PostForm.Get("quota"))
			if err != nil {
				return 0, nil, httpapi.NewError(400, "可注册人数必须是整数")
			}
			invitation, err := h.store.CreateInvitation(r.PostForm.Get("label"), quota, actor)
			if err != nil {
				return 0, nil, err
			}
			code = invitation.Code
		} else {
			id := r.PostForm.Get("id")
			if !invitationID.MatchString(id) {
				return 0, nil, httpapi.NewError(400, "邀请码标识无效")
			}
			if err := h.store.RevokeInvitation(id, actor); err != nil {
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
	var body bytes.Buffer
	if err = invitationTemplate.Execute(&body, struct {
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
