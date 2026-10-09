package storage

import (
	"net/http"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// The authenticated control consumes this durable queue. Only successful
// scheduled scans are eligible; manual, failed and incremental scans never run AI.
func (s *Handler) scheduledAnalysis(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "自动分析需要管理员权限")
	}
	if r.Method == "GET" {
		jobs, err := platform.Rows(s.DB.SQL, `SELECT id,analysis_user_id FROM jobs
WHERE trigger='scheduled' AND status='completed' AND analysis_status='pending'
ORDER BY created_at,id LIMIT 1`)
		return 200, object{"jobs": jobs}, err
	}
	if r.Method == "POST" {
		body, err := httpapi.RequestBody(w, r)
		if err != nil {
			return 0, nil, err
		}
		id, status, message := httpapi.FieldString(body, "id"), httpapi.FieldString(body, "status"), httpapi.FieldString(body, "error")
		if len(body) != 3 || len(id) != 32 || !jobRoute.MatchString("/api/jobs/"+id) || (status != "completed" && status != "failed") || len(message) > 4000 {
			return 0, nil, httpapi.NewError(400, "自动分析结果无效")
		}
		_, err = s.DB.SQL.Exec("UPDATE jobs SET analysis_status=?,analysis_error=? WHERE id=? AND status='completed' AND trigger='scheduled' AND analysis_status='pending'", status, message, id)
		return 200, object{"ok": true}, err
	}
	return 0, nil, httpapi.NewError(405, "自动分析队列不支持此操作")
}
