package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"project-alpha/internal/httpapi"
)

const remoteTimeout = 55 * time.Second

// remoteRecords keeps tool execution on the selected worker. Models, keys,
// conversations, authorization and scheduling all belong to the control.
type remoteRecords struct {
	control        *Control
	nodeID, userID string
}

func (s *remoteRecords) call(ctx context.Context, body any) (map[string]any, error) {
	user, err := s.control.agentUser(s.nodeID, s.userID)
	if err != nil {
		return nil, err
	}
	node, err := s.control.node(s.nodeID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	var result map[string]any
	err = s.control.call(ctx, node, "POST", "/api/worker/records", body, user, &result)
	return result, err
}

func (s *remoteRecords) Job(id string) (map[string]any, error) {
	return s.call(context.Background(), map[string]any{"operation": "job", "id": id})
}
func (s *remoteRecords) StartOverview(actor string) (map[string]any, error) {
	return s.call(context.Background(), map[string]any{"operation": "start"})
}
func (s *remoteRecords) Cancel(id, actor string) (map[string]any, error) {
	return s.call(context.Background(), map[string]any{"operation": "cancel", "id": id})
}
func (s *remoteRecords) Query(id, operation string, fields map[string]json.RawMessage) (map[string]any, error) {
	return s.call(context.Background(), map[string]any{"operation": "query", "id": id, "query": operation, "fields": fields})
}
func (s *remoteRecords) Explore(actor, id, path string, revision int64, depth int) (map[string]any, error) {
	return s.call(context.Background(), map[string]any{"operation": "explore", "id": id, "path": path, "revision": revision, "depth": depth})
}
func (s *remoteRecords) Wait(ctx context.Context, id string, check func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		result, err := s.call(ctx, map[string]any{"operation": "wait", "id": id})
		if err != nil {
			return err
		}
		if result["done"] == true {
			return nil
		}
	}
}
func (s *remoteRecords) DeleteReportPaths(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
	return s.deletePaths(ctx, id, paths, password, result, false)
}
func (s *remoteRecords) DeleteHostReportPaths(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
	return s.deletePaths(ctx, id, paths, password, result, true)
}
func (s *remoteRecords) deletePaths(ctx context.Context, id string, paths []string, password []byte, result func(string, string, string) error, host bool) error {
	defer clear(password)
	user, err := s.control.agentUser(s.nodeID, s.userID)
	if err != nil {
		return err
	}
	node, err := s.control.node(s.nodeID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Recheck the central identity during a long cleanup, cancelling the worker
	// connection if the account or node is revoked.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.control.agentUser(s.nodeID, s.userID); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	response, err := s.control.request(ctx, node, "POST", "/api/worker/records", map[string]any{"operation": "cleanup", "id": id, "paths": paths, "password": string(password), "host": host}, user)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1024*1024))
	for {
		var event struct {
			Path, Status, Message string
			Done                  bool
			Error                 string
			Code                  int
		}
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("节点清理连接中断，未确认结果：%w", err)
		}
		if event.Done {
			if event.Error == "" {
				return nil
			}
			if event.Code != 0 {
				return httpapi.NewError(event.Code, event.Error)
			}
			return fmt.Errorf("%s", event.Error)
		}
		if err := result(event.Path, event.Status, event.Message); err != nil {
			return err
		}
	}
}
