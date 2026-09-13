package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"project-alpha/internal/fsutil"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type toolSpec struct {
	Name, Description string
	Properties        object
}

func agentToolSpecs() []toolSpec {
	str := func(description string) object { return object{"type": "string", "description": description} }
	page := func() object {
		return object{"offset": object{"type": "integer", "minimum": 0, "description": "分页起点，首批填 0"}, "limit": object{"type": "integer", "minimum": 1, "maximum": 50, "description": "返回条数，最多 50"}}
	}
	containers := page()
	containers["query"] = str("容器名、状态或所属用户筛选；不筛选填空字符串")
	containers["sort_by"] = object{"type": "string", "enum": []string{"exclusive", "writable", "docker_logical"}}
	directory := page()
	directory["path"] = str("宿主机物理绝对路径；可写层使用 get_container 返回的 upper_path；从 / 查看全盘")
	return []toolSpec{
		{"get_overview", "读取本次分析的全盘快照摘要、设备对账、容器大头与完整性。不触发扫描。", object{}},
		{"list_containers", "查询与页面相同口径的容器用量。exclusive 为独占；shared 为共享引用，不能跨行相加。", containers},
		{"list_owners", "按去重归属用量查询用户。shared 为跨用户引用，不能加进用户独占排行。", page()},
		{"get_container", "查看一个容器的可写层、挂载、日志源及扫描状态，取得可用于下钻的物理路径。", object{"container": str("容器完整 ID 或精确名称")}},
		{"get_directory", "从本会话已有快照中按实际占用列出目录明细，优先使用已缓存的细查结果。不扫描；折叠或未记录时会明确提示。", directory},
		{"scan_directory", "对一个物理目录做只读局部扫描，返回目录明细、最大文件、按扩展名统计和 mtime 分布。仅在已有明细不够时调用。相同目录结果缓存 10 分钟；不会重扫其他容器。", object{"path": str("宿主机物理绝对路径；禁止 merged、虚拟文件系统及排除目录"), "refresh": object{"type": "boolean", "description": "通常 false，用户明确要求更新时 true"}}},
	}
}
func agentToolDefinitions(protocol string) []object {
	result := []object{}
	for _, s := range agentToolSpecs() {
		required := []string{}
		for k := range s.Properties {
			required = append(required, k)
		}
		sort.Strings(required)
		f := object{"name": s.Name, "description": s.Description, "strict": true, "parameters": object{"type": "object", "properties": s.Properties, "required": required, "additionalProperties": false}}
		if protocol == "completions" {
			result = append(result, object{"type": "function", "function": f})
		} else {
			f["type"] = "function"
			result = append(result, f)
		}
	}
	return result
}

type agentTools struct {
	agent                    *AgentManager
	sessionID, userID, actor string
	snapshot                 *Snapshot
	usage                    *usageIndex
	config                   Config
	detailScans              int
}

func (t *agentTools) overview() object {
	containers := t.usage.rankedContainers()
	top := []object{}
	for i, r := range containers {
		if i >= 10 {
			break
		}
		top = append(top, containerSummary(r))
	}
	owners := t.usage.rankedOwners()
	if len(owners) > 20 {
		owners = owners[:20]
	}
	warnings := t.snapshot.Warnings
	if len(warnings) > 20 {
		warnings = warnings[:20]
	}
	return object{"snapshot_id": t.snapshot.JobID, "observed_at": t.snapshot.FinishedAt, "filesystems": t.snapshot.Filesystems, "allocated": t.snapshot.Tree.Allocated, "files": t.snapshot.Tree.Files, "containers": len(containers), "exclusive": t.usage.Exclusive, "shared": t.usage.Shared, "cross_owner_shared": t.usage.CrossOwner, "unrelated": t.usage.Unrelated, "attribution_limited": t.usage.Limited, "scan": t.snapshot.Scan, "warnings": warnings, "warning_count": len(t.snapshot.Warnings), "top_containers": top, "top_owners": owners, "note": "统计为扫描期间观察值，非实时；全盘合计含可写层，不能重复相加。未解释空间不等于可回收空间。"}
}
func containerSummary(r *ContainerUsage) object {
	return object{"id": r.Container.ID, "name": r.Container.Name, "owner": ownerName(r.Container), "state": r.Container.State, "exclusive": r.Exclusive, "shared": r.Shared, "known": r.Known, "partial": r.Partial, "writable_layer": r.Container.WritableLayer, "docker_logical": r.Container.SizeRW}
}

func nodeSummary(n *Node) object {
	if n == nil {
		return object{"known": false, "reason": "快照未保留该路径；可以 scan_directory 补查，不能据此判为不存在或零占用"}
	}
	return object{"name": n.Name, "path": n.Path, "kind": n.Kind, "allocated": n.Allocated, "apparent": n.Apparent, "files": n.Files, "errors": n.Errors, "excluded_entries": n.Excluded, "permission_denied": n.PermissionDenied, "omitted_entries": n.Omitted, "omitted_references": n.OmittedReferences, "reference": n.Reference, "reason": n.Reason, "known": n.Kind != "unreadable" && n.Kind != "excluded"}
}
func directoryResult(s *Snapshot, u *usageIndex, path string, offset, limit int) object {
	n := u.resolve(path)
	result := object{"snapshot_id": s.JobID, "observed_at": s.FinishedAt, "requested_path": path, "node": nodeSummary(n), "offset": offset, "limit": limit}
	if n == nil {
		return result
	}
	children := append([]*Node{}, n.Children...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].Allocated == children[j].Allocated {
			return children[i].Path < children[j].Path
		}
		return children[i].Allocated > children[j].Allocated
	})
	result["total_entries"] = len(children)
	residual := n.Allocated
	for _, child := range children {
		residual -= child.Allocated
	}
	result["self_and_omitted_allocated"] = max(int64(0), residual)
	entries := []object{}
	for i := min(offset, len(children)); i < min(offset+limit, len(children)); i++ {
		child := children[i]
		entry := nodeSummary(child)
		if child.Kind == "reference" {
			entry["reference_target"] = nodeSummary(u.resolve(child.Path))
		}
		entries = append(entries, entry)
	}
	result["entries"] = entries
	result["has_more"] = offset+limit < len(children)
	return result
}
func (t *agentTools) call(ctx context.Context, name, arguments string) (any, error) {
	var spec *toolSpec
	for _, s := range agentToolSpecs() {
		if s.Name == name {
			v := s
			spec = &v
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("未知工具 %q", name)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &fields) != nil || fields == nil {
		return nil, fmt.Errorf("工具参数必须为 JSON 对象")
	}
	for key := range fields {
		if _, ok := spec.Properties[key]; !ok {
			return nil, fmt.Errorf("未知参数 %q", key)
		}
	}
	for key := range spec.Properties {
		if v, ok := fields[key]; !ok || string(v) == "null" {
			return nil, fmt.Errorf("缺少参数 %q", key)
		}
	}
	var args struct {
		Path, Container, Query string
		SortBy                 string `json:"sort_by"`
		Offset, Limit          int
		Refresh                bool
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, fmt.Errorf("工具参数类型无效")
	}
	if _, ok := spec.Properties["limit"]; ok && (args.Limit < 1 || args.Limit > 50 || args.Offset < 0 || args.Offset > 1000000) {
		return nil, fmt.Errorf("分页 limit 需为 1–50，offset 需为 0–1000000")
	}
	switch name {
	case "get_overview":
		return t.overview(), nil
	case "list_containers":
		if args.SortBy != "exclusive" && args.SortBy != "writable" && args.SortBy != "docker_logical" {
			return nil, fmt.Errorf("无效排序字段")
		}
		rows := []*ContainerUsage{}
		q := strings.ToLower(args.Query)
		for _, r := range t.usage.rankedContainers() {
			c := r.Container
			if q == "" || strings.Contains(strings.ToLower(c.Name+" "+c.Owner+" "+c.State), q) {
				rows = append(rows, r)
			}
		}
		value := func(r *ContainerUsage) int64 {
			if args.SortBy == "writable" {
				if r.Container.WritableLayer != nil && r.Container.WritableLayer.Allocated != nil {
					return *r.Container.WritableLayer.Allocated
				}
				return -1
			}
			if args.SortBy == "docker_logical" {
				if r.Container.SizeRW != nil {
					return *r.Container.SizeRW
				}
				return -1
			}
			return r.Exclusive
		}
		sort.SliceStable(rows, func(i, j int) bool { return value(rows[i]) > value(rows[j]) })
		items := []object{}
		for i := min(args.Offset, len(rows)); i < min(args.Offset+args.Limit, len(rows)); i++ {
			items = append(items, containerSummary(rows[i]))
		}
		return object{"snapshot_id": t.snapshot.JobID, "total": len(rows), "items": items, "has_more": args.Offset+args.Limit < len(rows)}, nil
	case "list_owners":
		rows := t.usage.rankedOwners()
		return object{"snapshot_id": t.snapshot.JobID, "total": len(rows), "items": rows[min(args.Offset, len(rows)):min(args.Offset+args.Limit, len(rows))], "cross_owner_shared": t.usage.CrossOwner}, nil
	case "get_container":
		var row *ContainerUsage
		for _, r := range t.usage.Containers {
			if r.Container.ID == args.Container || r.Container.Name == args.Container {
				row = r
				break
			}
		}
		if row == nil {
			return nil, fmt.Errorf("容器不存在于本次快照")
		}
		c := row.Container
		sources := []object{}
		for _, r := range t.snapshot.Resources {
			for _, id := range r.Containers {
				if id == c.ID {
					sources = append(sources, object{"path": r.Path, "kinds": r.Kinds, "referencing_containers": r.Containers, "node": nodeSummary(t.usage.resolve(r.Path))})
					break
				}
			}
		}
		return object{"snapshot_id": t.snapshot.JobID, "observed_at": t.snapshot.FinishedAt, "usage": row, "sources": sources, "note": "挂载源可能共享或互相包含，不能直接把 sources 相加"}, nil
	case "get_directory":
		path, err := validateDetailPath(args.Path, t.config, t.agent.db.Directory)
		if err != nil {
			return nil, err
		}
		details, err := platform.Rows(t.agent.db.SQL, "SELECT path,job_id FROM agent_details WHERE session_id=? ORDER BY length(path) DESC", t.sessionID)
		if err != nil {
			return nil, err
		}
		for _, detail := range details {
			if within(path, httpapi.String(detail["path"])) {
				s, err := t.agent.loadSnapshot(httpapi.String(detail["job_id"]))
				if err != nil {
					continue
				}
				u := buildUsage(s)
				if u.resolve(path) != nil {
					return directoryResult(s, u, path, args.Offset, args.Limit), nil
				}
			}
		}
		return directoryResult(t.snapshot, t.usage, path, args.Offset, args.Limit), nil
	case "scan_directory":
		path, err := validateDetailPath(args.Path, t.config, t.agent.db.Directory)
		if err != nil {
			return nil, err
		}
		// The tool may only inspect paths inside this conversation's baseline roots.
		covered := false
		for _, root := range t.config.Root {
			if within(path, fsutil.Canonical(root)) {
				covered = true
			}
		}
		for _, r := range t.snapshot.Resources {
			if within(path, r.Path) {
				covered = true
			}
		}
		if !covered {
			return nil, fmt.Errorf("路径不在本次全盘快照的扫描范围内")
		}
		var jobID string
		cached := false
		if !args.Refresh {
			err = t.agent.db.SQL.QueryRow("SELECT d.job_id FROM agent_details d JOIN jobs j ON j.id=d.job_id WHERE d.session_id=? AND d.path=? AND j.status='completed' AND j.finished_at>?", t.sessionID, path, platform.Now()-600).Scan(&jobID)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			cached = err == nil
		}
		if !cached {
			if t.detailScans >= 6 {
				return nil, fmt.Errorf("本轮已完成 6 次目录细查，请复用结果或在下一轮继续")
			}
			t.detailScans++
			c := t.config
			c.Root = []string{path}
			c.IncludeDockerRoot = false
			c.MaxDepth = 3
			c.MaxNodes = 5000
			c.IntervalMinutes = 0
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			jobID, err = t.agent.scan(ctx, t.sessionID, t.userID, t.actor, "agent-detail", scanPlan{Config: c, AnalysisPath: path})
			if err != nil {
				return nil, err
			}
			_, err = t.agent.db.SQL.Exec("INSERT INTO agent_details(session_id,path,job_id) VALUES(?,?,?) ON CONFLICT(session_id,path) DO UPDATE SET job_id=excluded.job_id", t.sessionID, path, jobID)
			if err != nil {
				return nil, err
			}
		}
		s, err := t.agent.loadSnapshot(jobID)
		if err != nil {
			return nil, err
		}
		u := buildUsage(s)
		if u.resolve(path) == nil && len(s.Tree.Children) == 1 {
			path = s.Tree.Children[0].Path
		}
		result := directoryResult(s, u, path, 0, 30)
		result["analysis"] = s.Analysis
		result["cached"] = cached
		result["warnings"] = s.Warnings
		result["scan"] = s.Scan
		result["note"] = "这是局部扫描的新观察，不能加到原全盘总量；文件类型只是后缀分组，最大文件列表最多 40 项。"
		return result, nil
	}
	return nil, fmt.Errorf("工具未实现")
}
