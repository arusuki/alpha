package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"project-alpha/internal/httpapi"
)

// A view carries server-computed accounting and only the open directory's
// immediate entries. Nodes used by source selectors are summaries, never trees.
func snapshotView(s *Snapshot, u *usageIndex, path string) object {
	if path == "" {
		path = s.Tree.Path
	}
	nodes := map[string]object{}
	var add func(*Node)
	add = func(n *Node) {
		if n == nil || nodes[n.Path] != nil {
			return
		}
		value := nodeSummary(n)
		value["children"] = []string{}
		value["partial"] = u.Incomplete[n.Path]
		value["host"] = object{"allocated": u.HostAllocated[n.Path], "apparent": u.HostApparent[n.Path], "visible": u.HostVisible[n.Path]}
		nodes[n.Path] = value
		if n.Kind == "reference" {
			add(u.Nodes[n.Reference])
		}
	}
	open := func(n *Node) {
		if n == nil {
			return
		}
		add(n)
		children := []string{}
		for _, child := range n.Children {
			add(child)
			children = append(children, child.Path)
		}
		nodes[n.Path]["children"] = children
		nodes[n.Path]["loaded"] = true
	}
	open(s.Tree)
	for _, c := range s.Containers {
		if c.UpperPath != nil {
			add(u.Nodes[*c.UpperPath])
		}
		if c.LogPath != nil {
			add(u.Nodes[filepath.Dir(*c.LogPath)])
		}
		for _, m := range c.Mounts {
			if m.Source != nil {
				add(u.Nodes[*m.Source])
			}
		}
	}
	for _, resource := range s.Resources {
		add(u.Nodes[resource.Path])
	}
	add(u.Nodes[path])
	open(u.resolve(path))
	// Keep breadcrumb nodes available without loading their sibling subtrees.
	for parent := filepath.Dir(path); filepath.IsAbs(parent); parent = filepath.Dir(parent) {
		add(u.Nodes[parent])
		if parent == "/" {
			break
		}
	}
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]object, 0, len(nodes))
	for _, key := range keys {
		rows = append(rows, nodes[key])
	}
	return object{
		"view_version": 1, "root": s.Tree.Path, "nodes": rows,
		"metadata": object{"job_id": s.JobID, "revision": s.Revision, "host": s.Host, "finished_at": s.FinishedAt, "updated_at": s.UpdatedAt,
			"docker": s.Docker, "scan": s.Scan, "containers": s.Containers, "resources": s.Resources, "filesystems": s.Filesystems, "warnings": s.Warnings},
		"usage": object{"exclusive": u.Exclusive, "shared": u.Shared, "crossOwner": u.CrossOwner, "unrelated": u.Unrelated,
			"attributionLimited": u.Limited, "containers": u.Containers, "owners": u.Owners},
	}
}

func (s *Handler) serveSnapshotView(w http.ResponseWriter, r *http.Request, id string) (int, any, error) {
	for key, values := range r.URL.Query() {
		if key != "path" || len(values) != 1 || len(values[0]) > 4096 {
			return 0, nil, httpapi.NewError(400, "展示查询参数无效")
		}
	}
	view, err := s.readSnapshotView(r, id)
	if err != nil {
		return 0, nil, err
	}
	snapshot := view.snapshot
	path := r.URL.Query().Get("path")
	if path != "" && path != snapshot.Tree.Path && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
		return 0, nil, httpapi.NewError(400, "目录路径无效")
	}
	return 200, snapshotView(snapshot, view.usage, path), nil
}

// Retain at most one immutable parsed record per handler. Directory navigation
// reuses accounting; publications, owner edits and baseline replacement invalidate
// it. Authentication and job existence are checked before every cache lookup.
type snapshotViewCache struct {
	mu   sync.Mutex
	key  string
	view *recordView
}

func (s *Handler) readSnapshotView(r *http.Request, id string) (*recordView, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	job, err := s.Job(id)
	if err != nil {
		return nil, err
	}
	if job["status"] != "completed" {
		return nil, httpapi.NewError(409, "该任务尚未生成完整结果")
	}
	var revision int64
	var stored bool
	err = s.DB.SQL.QueryRowContext(r.Context(), "SELECT coalesce((SELECT revision FROM snapshot_records WHERE job_id=?),0),EXISTS(SELECT 1 FROM snapshot_records WHERE job_id=?)", id, id).Scan(&revision, &stored)
	if err != nil {
		return nil, err
	}
	owners, err := s.DB.owners()
	if err != nil {
		return nil, err
	}
	ownerKey, err := json.Marshal(owners)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s:%t:%d:%s", id, stored, revision, ownerKey)
	if !stored {
		info, err := os.Stat(filepath.Join(s.DB.Directory, "results", id, "snapshot.json"))
		if err != nil {
			return nil, httpapi.NewError(503, "该扫描结果文件无法读取")
		}
		key += fmt.Sprintf(":%d:%d", info.ModTime().UnixNano(), info.Size())
	}
	if s.views.key == key && s.views.view != nil {
		return s.views.view, nil
	}
	snapshot, err := s.ReadSnapshot(id)
	if err != nil {
		return nil, err
	}
	view := &recordView{snapshot: snapshot, usage: buildUsage(snapshot)}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	s.views.key, s.views.view = key, view
	return view, nil
}
