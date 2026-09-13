package storage

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

var agentSessionRoute = regexp.MustCompile(`^/api/agent/sessions/([a-f0-9]{32})(/messages|/cancel)?$`)

func (s *Handler) agentDispatch(w http.ResponseWriter, r *http.Request, userID, actor string) (int, any, error) {
	fail := func(err error) (int, any, error) { return 0, nil, err }
	a := s.Manager.Agent
	if r.URL.Path == "/api/agent/settings" {
		if r.Method == "GET" {
			c, revision, err := s.DB.agentConfig()
			return 200, publicAgentConfig(c, revision), err
		}
		if r.Method == "PUT" {
			body, err := httpapi.RequestBody(w, r)
			if err != nil {
				return fail(err)
			}
			value, err := s.DB.saveAgentConfig(body, actor)
			return 200, value, err
		}
	}
	if r.URL.Path == "/api/agent/sessions" {
		if r.Method == "GET" {
			sessions, err := platform.Rows(s.DB.SQL, "SELECT id,title,status,created_at,updated_at,snapshot_id,provider,model,error FROM agent_sessions WHERE user_id=? ORDER BY updated_at DESC LIMIT 50", userID)
			return 200, object{"sessions": sessions}, err
		}
		if r.Method == "POST" {
			body, err := httpapi.RequestBody(w, r)
			if err != nil {
				return fail(err)
			}
			if len(body) != 1 {
				return fail(httpapi.NewError(400, "仅接受 message 参数"))
			}
			session, err := a.start("", userID, actor, httpapi.FieldString(body, "message"))
			return 202, session, err
		}
	}
	if match := agentSessionRoute.FindStringSubmatch(r.URL.Path); match != nil {
		id, action := match[1], match[2]
		if r.Method == "GET" && action == "" {
			session, err := a.session(id, userID)
			if err != nil {
				return fail(err)
			}
			after := int64(0)
			if value := r.URL.Query().Get("after"); value != "" {
				after, err = strconv.ParseInt(value, 10, 64)
				if err != nil || after < 0 {
					return fail(httpapi.NewError(400, "消息游标无效"))
				}
			}
			messages, err := platform.Rows(s.DB.SQL, "SELECT id,role,content,tool_name,created_at FROM agent_messages WHERE session_id=? AND id>? ORDER BY id LIMIT 201", id, after)
			if err != nil {
				return fail(err)
			}
			hasMore := len(messages) > 200
			if hasMore {
				messages = messages[:200]
			}
			if len(messages) > 0 {
				after = numberInt64(messages[len(messages)-1]["id"])
			}
			var job any
			if jobID := httpapi.String(session["active_job_id"]); jobID != "" {
				job, err = s.DB.job(jobID)
				if err != nil {
					return fail(err)
				}
			}
			return 200, object{"session": session, "messages": messages, "next_after": after, "has_more": hasMore, "active_job": job}, nil
		}
		if r.Method == "POST" {
			body, err := httpapi.RequestBody(w, r)
			if err != nil {
				return fail(err)
			}
			if action == "/messages" {
				if len(body) != 1 {
					return fail(httpapi.NewError(400, "仅接受 message 参数"))
				}
				session, err := a.start(id, userID, actor, httpapi.FieldString(body, "message"))
				return 202, session, err
			}
			if action == "/cancel" {
				value, err := a.stop(id, userID)
				return 200, value, err
			}
		}
	}
	return fail(httpapi.NewError(404, "Agent 接口不存在"))
}

func isAgentRoute(path string) bool { return strings.HasPrefix(path, "/api/agent/") }
