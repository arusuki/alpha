package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// DispatchTools is exposed only behind the worker's service-token boundary.
// It is never part of the browser proxy allowlist or a model's tool schema.
func (s *Handler) DispatchTools(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "节点工具需要管理员权限")
	}
	fields, err := httpapi.RequestBody(w, r)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		for _, raw := range fields {
			clear(raw)
		}
	}()
	raw, err := json.Marshal(fields)
	if err != nil {
		return 0, nil, err
	}
	defer clear(raw)
	var args struct {
		Operation string                     `json:"operation"`
		ID        string                     `json:"id"`
		Query     string                     `json:"query"`
		Fields    map[string]json.RawMessage `json:"fields"`
		Path      string                     `json:"path"`
		Revision  int64                      `json:"revision"`
		Depth     int                        `json:"depth"`
		Paths     []string                   `json:"paths"`
		Password  string                     `json:"password"`
		Host      bool                       `json:"host"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&args) != nil || args.Operation != "start" && (len(args.ID) != 32 || !jobRoute.MatchString("/api/jobs/"+args.ID)) {
		return 0, nil, httpapi.NewError(400, "节点工具参数无效")
	}
	var value any
	switch args.Operation {
	case "start":
		value, err = s.StartOverview(user.Username)
	case "job":
		value, err = s.Job(args.ID)
	case "cancel":
		value, err = s.Cancel(args.ID, user.Username)
	case "query":
		value, err = s.Query(args.ID, args.Query, args.Fields)
	case "explore":
		value, err = s.Explore(user.Username, args.ID, args.Path, args.Revision, args.Depth)
	case "wait":
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		err = s.Wait(ctx, args.ID, nil)
		value = map[string]bool{"done": err == nil}
		if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
			err = nil
		}
	case "cleanup":
		password := []byte(args.Password)
		args.Password = ""
		defer clear(password)
		if len(args.Paths) < 1 || len(args.Paths) > 100 || len(password) == 0 || len(password) > 1024 || bytes.IndexAny(password, "\x00\r\n") >= 0 {
			return 0, nil, httpapi.NewError(400, "清理路径或密码参数无效")
		}
		// Chunked results distinguish confirmed deletions from a lost connection.
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Time{})
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if err := controller.Flush(); err != nil {
			return 0, nil, nil
		}
		encoder := json.NewEncoder(w)
		publish := func(value any) error {
			if err := encoder.Encode(value); err != nil {
				return err
			}
			return controller.Flush()
		}
		remove := s.DeleteReportPaths
		if args.Host {
			remove = s.DeleteHostReportPaths
		}
		err = remove(r.Context(), user.Username, args.ID, args.Paths, password, func(path, status, message string) error {
			return publish(map[string]any{"path": path, "status": status, "message": message})
		})
		message, code := "", 0
		if err != nil {
			message = err.Error()
			var apiErr *httpapi.Error
			if errors.As(err, &apiErr) {
				code = apiErr.Status
			}
		}
		_ = publish(map[string]any{"done": true, "error": message, "code": code})
		return 0, nil, nil
	default:
		return 0, nil, httpapi.NewError(404, "节点工具不存在")
	}
	return 200, value, err
}
