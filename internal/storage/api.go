package storage

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Handler exposes storage routes through the authenticated platform server.
type Handler struct {
	DB      *Store
	Manager *Manager
}

func NewHandler(db *Store, manager *Manager) *Handler { return &Handler{DB: db, Manager: manager} }

var jobRoute = regexp.MustCompile(`^/api/jobs/([a-f0-9]{32})(/cancel|/snapshot|/expand|/changes|/events)?$`)

func (s *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	route, method := r.URL.Path, r.Method
	db := s.DB
	var err error
	admin := user.Role == "admin"
	failure := func(err error) (int, any, error) { return 0, nil, err }
	if route == "/api/settings" && !admin {
		return failure(httpapi.NewError(403, "此操作需要管理员权限"))
	}
	if isAgentRoute(route) {
		if !admin {
			return failure(httpapi.NewError(403, "Agent 分析需要管理员权限"))
		}
		return s.agentDispatch(w, r, user.ID, user.Username)
	}
	if method == "GET" && route == "/api/state" {
		jobs, err := db.jobs(platform.Now() + 1)
		if err != nil {
			return failure(err)
		}
		latest, err := db.latest()
		if err != nil {
			return failure(err)
		}
		config, err := db.config()
		if err != nil {
			return failure(err)
		}
		var active any
		directories, err := db.directoryJobs()
		if err != nil {
			return failure(err)
		}
		for _, j := range append(append([]object{}, directories...), jobs...) {
			if activeStatus(j["status"].(string)) {
				active = j
				break
			}
		}
		return 200, object{"jobs": jobs, "directory_jobs": directories, "latest_id": latest, "active": active, "interval_minutes": config.Value.IntervalMinutes}, nil
	}
	if method == "GET" && route == "/api/jobs" {
		before := platform.Now() + 1
		if values, ok := r.URL.Query()["before"]; ok {
			before, err = strconv.ParseFloat(values[0], 64)
			if err != nil || math.IsNaN(before) || math.IsInf(before, 0) {
				return failure(httpapi.NewError(400, "历史游标无效"))
			}
		}
		jobs, err := db.jobs(before)
		return 200, object{"jobs": jobs}, err
	}
	if route == "/api/settings" {
		if method == "GET" {
			value, err := db.config()
			return 200, value, err
		}
		if method == "PUT" {
			value, err := httpapi.RequestBody(w, r)
			if err != nil {
				return failure(err)
			}
			var revision int64
			if string(value["revision"]) == "null" || json.Unmarshal(value["revision"], &revision) != nil {
				return failure(httpapi.NewError(400, "缺少配置版本"))
			}
			c, err := parseConfig(value["value"])
			if err != nil {
				return failure(err)
			}
			result, err := db.saveConfig(c, revision, user.Username)
			return 200, result, err
		}
	}
	if method == "POST" && route == "/api/jobs" {
		if _, err = httpapi.RequestBody(w, r); err != nil {
			return failure(err)
		}
		job, err := s.Manager.Start(user.Username, "manual")
		return 202, job, err
	}
	if match := jobRoute.FindStringSubmatch(route); match != nil {
		id, action := match[1], match[2]
		if method == "DELETE" && action == "" {
			result, err := s.Manager.Delete(id, user.Username)
			return 200, result, err
		}
		if method == "GET" && action == "/events" {
			return s.streamSnapshot(w, r, id)
		}
		if method == "POST" && action == "/expand" {
			value, err := httpapi.RequestBody(w, r)
			if err != nil {
				return failure(err)
			}
			var revision int64
			if (len(value) != 2 && len(value) != 3) || value["revision"] == nil || string(value["revision"]) == "null" || json.Unmarshal(value["revision"], &revision) != nil || revision < 0 || httpapi.FieldString(value, "path") == "" {
				return failure(httpapi.NewError(400, "请提供目录路径和扫描记录版本"))
			}
			depth := incrementalDepth
			if len(value) == 3 {
				raw, ok := value["depth"]
				if !ok || string(raw) == "null" || json.Unmarshal(raw, &depth) != nil {
					return failure(httpapi.NewError(400, "每次分析深度必须为 1–3 层"))
				}
			}
			job, err := s.Manager.StartIncremental(user.Username, id, httpapi.FieldString(value, "path"), revision, depth)
			return 202, job, err
		}
		if method == "POST" && action == "/cancel" {
			if _, err = httpapi.RequestBody(w, r); err != nil {
				return failure(err)
			}
			job, err := s.Manager.Cancel(id, user.Username)
			return 200, job, err
		}
		if method == "GET" && action == "" {
			job, err := db.job(id)
			return 200, job, err
		}
		if method == "GET" && action == "/snapshot" {
			snapshot, err := s.snapshot(id)
			return 200, snapshot, err
		}
		if method == "GET" && action == "/changes" {
			values := r.URL.Query()["revision"]
			if len(values) != 1 {
				return failure(httpapi.NewError(400, "请提供当前扫描记录版本"))
			}
			revision, err := strconv.ParseInt(values[0], 10, 64)
			if err != nil || revision < 0 {
				return failure(httpapi.NewError(400, "扫描记录版本无效"))
			}
			changes, err := s.snapshotChanges(id, revision)
			return 200, changes, err
		}
	}
	if method == "GET" && route == "/api/snapshot" {
		id, err := db.latest()
		if err != nil {
			return failure(err)
		}
		if id == nil {
			return failure(httpapi.NewError(404, "还没有完成的扫描，请先在平台发起扫描"))
		}
		snapshot, err := s.snapshot(*id)
		return 200, snapshot, err
	}
	if method == "PUT" && route == "/api/owners" {
		value, err := httpapi.RequestBody(w, r)
		if err != nil {
			return failure(err)
		}
		var owner string
		if json.Unmarshal(value["owner"], &owner) != nil || string(value["owner"]) == "null" {
			return failure(httpapi.NewError(400, "容器标识或所属用户无效"))
		}
		if err = db.setOwner(httpapi.FieldString(value, "container_id"), owner, user.Username); err != nil {
			return failure(err)
		}
		owners, err := db.owners()
		return 200, object{"owners": owners}, err
	}
	return failure(httpapi.NewError(404, "接口不存在"))
}
func (s *Handler) snapshot(id string) (object, error) {
	snapshot, err := s.DB.readSnapshot(id)
	if err != nil {
		return nil, err
	}
	var result object
	snapshot.Accounting = nil
	if json.Unmarshal([]byte(httpapi.JSONText(snapshot)), &result) != nil || result == nil {
		return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
	}
	return s.publicSnapshot(result, id, snapshot.Revision)
}
func (s *Handler) publicSnapshot(result object, id string, revision int64) (object, error) {
	owners, err := s.DB.owners()
	if err != nil {
		return nil, err
	}
	containers, ok := result["containers"].([]any)
	if !ok {
		return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
	}
	for _, v := range containers {
		c, ok := v.(map[string]any)
		if !ok {
			return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
		}
		c["label_owner"] = c["owner"]
		if owner, ok := owners[httpapi.String(c["id"])]; ok {
			c["owner"] = owner
		}
	}
	delete(result, "incremental_accounting")
	result["revision"] = revision
	result["job_id"] = id
	return result, nil
}
