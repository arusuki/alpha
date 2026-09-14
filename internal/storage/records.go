package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"project-alpha/internal/httpapi"
)

// Service is the shared entry point for record reads and incremental exploration.
// HTTP handlers and agent tools use the same revisions, ownership and scan lock.
type Service struct {
	DB      *Store
	Manager *Manager
}

func NewService(db *Store, manager *Manager) *Service    { return &Service{db, manager} }
func (s *Service) Job(id string) (map[string]any, error) { return s.DB.job(id) }
func (s *Service) Cancel(id, actor string) (map[string]any, error) {
	return s.Manager.Cancel(id, actor)
}
func (s *Service) Explore(actor, id, path string, revision int64, depth int) (map[string]any, error) {
	return s.Manager.StartIncremental(actor, id, path, revision, depth)
}
func (s *Service) StartOverview(actor string) (map[string]any, error) {
	settings, err := s.DB.config()
	if err != nil {
		return nil, err
	}
	return s.Manager.startPlan(actor, "agent-full", scanPlan{Config: fullScanConfig(settings.Value)})
}
func fullScanConfig(c Config) Config {
	c.Root = appendUnique(append([]string{}, c.Root...), "/")
	c.IncludeDockerRoot = !c.NoDocker
	c.MaxDepth = 3
	c.MaxNodes = 10000
	c.IntervalMinutes = 0
	return c
}

// ReadSnapshot applies the same owner overrides used by the web view.
func (s *Service) ReadSnapshot(id string) (*Snapshot, error) {
	snapshot, err := s.DB.readSnapshot(id)
	if err != nil {
		return nil, err
	}
	owners, err := s.DB.owners()
	if err != nil {
		return nil, err
	}
	for i := range snapshot.Containers {
		if owner, ok := owners[snapshot.Containers[i].ID]; ok {
			snapshot.Containers[i].Owner = owner
		}
	}
	return snapshot, nil
}
func (s *Service) Snapshot(id string) (object, error) {
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
func (s *Service) publicSnapshot(result object, id string, revision int64) (object, error) {
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

// Wait waits for both durable completion and worker exit. A completed record can
// be read before the process exits, but the next exploration still needs its lock.
// check lets a caller revalidate its authorization while waiting.
func (s *Service) Wait(ctx context.Context, id string, check func() error) error {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		s.Manager.mu.Lock()
		err := s.Manager.reapLocked()
		running := s.Manager.process != nil && s.Manager.jobID == id
		s.Manager.mu.Unlock()
		if err != nil {
			return err
		}
		job, err := s.Job(id)
		if err != nil {
			return err
		}
		status := httpapi.String(job["status"])
		if !running && !activeStatus(status) {
			if status == "completed" {
				return nil
			}
			return fmt.Errorf("扫描未完成：%s %s", status, httpapi.String(job["error"]))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
