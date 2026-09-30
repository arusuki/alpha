package agent

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Handler struct {
	DB      *Store
	Manager *Manager
}

func NewHandler(db *Store, manager *Manager) *Handler { return &Handler{db, manager} }

var agentSessionRoute = regexp.MustCompile(`^/api/agent/sessions/([a-f0-9]{32})(/messages|/cancel|/events|/retry)?$`)
var recordIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var cleanupRoute = regexp.MustCompile(`^/api/agent/cleanups/([a-f0-9]{32})(/delete|/cancel)?$`)

func (s *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "Agent 分析需要管理员权限")
	}
	userID, actor := user.ID, user.Username
	fail := func(err error) (int, any, error) { return 0, nil, err }
	a := s.Manager
	if r.URL.Path == "/api/agent/cleanup-reports" && r.Method == "GET" {
		value, err := a.cleanupReports(userID)
		return 200, value, err
	}
	if r.URL.Path == "/api/agent/cleanups" && r.Method == "POST" {
		body, err := httpapi.RequestBody(w, r)
		if err != nil {
			return fail(err)
		}
		var reportID int64
		if len(body) != 1 || json.Unmarshal(body["report_id"], &reportID) != nil || reportID < 1 {
			return fail(httpapi.NewError(400, "需要有效的 report_id"))
		}
		value, err := a.prepareCleanup(reportID, userID, actor)
		return 201, value, err
	}
	if match := cleanupRoute.FindStringSubmatch(r.URL.Path); match != nil {
		if r.Method == "GET" && match[2] == "" {
			value, err := a.cleanup(match[1], userID)
			return 200, value, err
		}
		if r.Method == "DELETE" && match[2] == "" {
			value, err := a.removeCleanup(match[1], userID, actor)
			return 200, value, err
		}
		if r.Method == "POST" && match[2] == "/cancel" {
			if _, err := httpapi.RequestBody(w, r); err != nil {
				return fail(err)
			}
			value, err := a.stopCleanup(match[1], userID)
			return 200, value, err
		}
		if r.Method == "POST" && match[2] == "/delete" {
			body, err := httpapi.RequestBody(w, r)
			if err != nil {
				return fail(err)
			}
			defer func() {
				for _, raw := range body {
					clear(raw)
				}
			}()
			var ids []string
			if len(body) != 2 || json.Unmarshal(body["entry_ids"], &ids) != nil || body["sudo_password"] == nil {
				return fail(httpapi.NewError(400, "需要 entry_ids 列表和本次操作的 sudo_password"))
			}
			password := []byte(httpapi.FieldString(body, "sudo_password"))
			clear(body["sudo_password"])
			value, err := a.deleteCleanup(match[1], userID, actor, ids, password)
			return 202, value, err
		}
	}
	if r.URL.Path == "/api/agent/reports" && r.Method == "POST" {
		body, err := httpapi.RequestBody(w, r)
		if err != nil {
			return fail(err)
		}
		var source reportSource
		if len(body) != 4 || body["concurrency"] == nil || body["snapshot_id"] == nil || body["revision"] == nil || body["scope"] == nil || string(body["revision"]) == "null" || json.Unmarshal([]byte(httpapi.JSONText(body)), &source) != nil || !recordIDPattern.MatchString(source.SnapshotID) || source.Revision < 0 || source.Concurrency < 1 || source.Concurrency > maxReportConcurrency || (source.Scope != "host" && source.Scope != "container") {
			return fail(httpapi.NewError(400, "需要有效的 snapshot_id、非负整数 revision、1–16 的整数 concurrency 和 host/container scope"))
		}
		request := diskReportRequest
		if source.Scope == "host" {
			request = hostReportRequest
		}
		session, err := a.start("", userID, actor, request, &source)
		return 202, session, err
	}
	if r.URL.Path == "/api/agent/settings" {
		if r.Method == "GET" {
			c, revision, err := s.DB.agentConfig()
			return 200, publicConfig(c, revision), err
		}
		if r.Method == "PUT" {
			body, err := httpapi.RequestBody(w, r)
			if err != nil {
				return fail(err)
			}
			value, err := s.DB.saveConfig(body, actor)
			return 200, value, err
		}
	}
	if r.URL.Path == "/api/agent/sessions" {
		if r.Method == "GET" {
			sessions, err := platform.Rows(s.DB.SQL, `SELECT id,title,status,created_at,updated_at,snapshot_id,provider,model,error,report_scope FROM (
 SELECT id,title,status,created_at,updated_at,snapshot_id,provider,model,error,report_scope,
 ROW_NUMBER() OVER (PARTITION BY report_scope ORDER BY updated_at DESC,id DESC) AS scope_rank
 FROM agent_sessions WHERE user_id=? AND node_id=?
) WHERE scope_rank<=50 ORDER BY updated_at DESC,id DESC`, userID, s.DB.NodeID)
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
			session, err := a.start("", userID, actor, httpapi.FieldString(body, "message"), nil)
			return 202, session, err
		}
	}
	if match := agentSessionRoute.FindStringSubmatch(r.URL.Path); match != nil {
		id, action := match[1], match[2]
		if r.Method == "GET" && (action == "" || action == "/events") {
			after := int64(0)
			value := r.URL.Query().Get("after")
			if action == "/events" && r.Header.Get("Last-Event-ID") != "" {
				value = r.Header.Get("Last-Event-ID")
			}
			if value != "" {
				var err error
				after, err = strconv.ParseInt(value, 10, 64)
				if err != nil || after < 0 {
					return fail(httpapi.NewError(400, "消息游标无效"))
				}
			}
			if action == "/events" {
				return s.streamSession(w, r, id, userID, after)
			}
			update, err := a.sessionUpdate(id, userID, after)
			return 200, update, err
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
				session, err := a.start(id, userID, actor, httpapi.FieldString(body, "message"), nil)
				return 202, session, err
			}
			if action == "/retry" {
				var targets []reportRetry
				if len(body) != 1 || json.Unmarshal(body["requests"], &targets) != nil || len(targets) == 0 {
					return fail(httpapi.NewError(400, "需要非空 requests 列表，每项包含 group_id 和 request_id"))
				}
				session, err := a.retryGroups(id, userID, actor, targets)
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

func IsRoute(path string) bool { return strings.HasPrefix(path, "/api/agent/") }
