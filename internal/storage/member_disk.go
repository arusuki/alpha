package storage

import (
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// This view never sends the host tree or other members' source paths to a member.
// Usage is computed before filtering so shared bytes keep their real attribution.
func (s *Handler) memberDisk(r *http.Request, user platform.User) (object, error) {
	owner, ok := strings.CutPrefix(user.Username, "member:")
	if !ok || owner == "" || user.Role != "viewer" {
		return nil, httpapi.NewError(403, "请通过使用者状态页查看磁盘用量")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, httpapi.NewError(400, "磁盘查询参数无效")
	}
	for key, values := range query {
		if len(values) != 1 || (key != "container" && key != "path" && key != "offset") || values[0] == "" {
			return nil, httpapi.NewError(400, "磁盘查询参数无效")
		}
	}
	offset := 0
	if query.Has("offset") {
		offset, err = strconv.Atoi(query.Get("offset"))
		if err != nil || offset < 0 || offset > 1000000 {
			return nil, httpapi.NewError(400, "目录分页参数无效")
		}
	}
	if (query.Has("path") && !query.Has("container")) || (query.Has("offset") && !query.Has("path")) {
		return nil, httpapi.NewError(400, "请先选择自己的容器")
	}
	id, err := s.DB.latest()
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, httpapi.NewError(404, "暂无磁盘扫描结果，请等待管理员完成节点扫描")
	}
	snapshot, err := s.ReadSnapshot(*id)
	if err != nil {
		return nil, err
	}
	return memberDiskView(snapshot, owner, query.Get("container"), query.Get("path"), offset)
}

func memberDiskView(snapshot *Snapshot, owner, container, path string, offset int) (object, error) {
	u := buildUsage(snapshot)
	result := object{"observed_at": snapshot.FinishedAt, "updated_at": snapshot.UpdatedAt}
	if container == "" {
		filesystems := []object{}
		for _, fs := range snapshot.Filesystems {
			filesystems = append(filesystems, object{"mount": fs["mount"], "fs": fs["fs"], "total": fs["total"], "used": fs["used"], "available": fs["available"]})
		}
		rows := []object{}
		for _, row := range u.rankedContainers() {
			c := row.Container
			rows = append(rows, object{"id": c.ID, "name": c.Name, "owner": ownerName(c), "exclusive": row.Exclusive, "shared": row.Shared, "known": row.Known, "partial": row.Partial, "expandable": ownerName(c) == owner})
		}
		result["filesystems"], result["containers"] = filesystems, rows
		result["exclusive"], result["shared"], result["unrelated"] = u.Exclusive, u.Shared, u.Unrelated
		return result, nil
	}
	row := u.Containers[container]
	if row == nil || ownerName(row.Container) != owner {
		return nil, httpapi.NewError(403, "只能展开自己的容器用量")
	}
	roots := []string{}
	entrances := snapshot.filesystemEntrances()
	for _, resource := range snapshot.Resources {
		if slices.Contains(resource.Containers, container) && !resource.accessOnly(entrances) && filepath.IsAbs(resource.Path) && resource.Path != "/" {
			roots = appendUnique(roots, resource.Path)
		}
	}
	allowed := func(path string) bool {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return false
		}
		for _, root := range roots {
			if within(path, root) {
				return true
			}
		}
		return false
	}
	// An inode reference may point outside the member's resource roots. Do not
	// expose that target or follow it into a different container/host directory.
	summary := func(path string) object {
		n := u.Nodes[path]
		value := object{"path": path, "name": filepath.Base(path), "known": false, "expandable": false}
		if n == nil {
			return value
		}
		resolved := u.resolve(path)
		canResolve := resolved != nil && allowed(resolved.Path)
		if canResolve {
			n = resolved
		}
		value["known"] = !n.SizeUnknown && !n.Scanning && n.Kind != "unreadable" && n.Kind != "excluded"
		// References navigate to their target, but occupy only their own
		// inode-deduplicated bytes in the parent directory's capacity map.
		value["allocated"] = u.Nodes[path].Allocated
		value["partial"] = u.Incomplete[n.Path] || n.Omitted > 0
		value["expandable"] = canResolve && n.Kind == "directory"
		return value
	}
	if path == "" {
		sources := []object{}
		for _, root := range roots {
			v := summary(root)
			v["label"] = "日志与元数据"
			if row.Container.UpperPath != nil && root == *row.Container.UpperPath {
				v["label"] = "可写层 /"
			}
			for _, mount := range row.Container.Mounts {
				if mount.Source != nil && *mount.Source == root {
					v["label"] = mount.Destination + " · " + mount.Type
				}
			}
			sources = append(sources, v)
		}
		result["sources"] = sources
		result["exclusive"], result["shared"] = row.Exclusive, row.Shared
		result["writable_layer"] = row.Container.WritableLayer
		return result, nil
	}
	if !allowed(path) {
		return nil, httpapi.NewError(403, "只能查看自己容器的用量目录")
	}
	n := u.resolve(path)
	if n != nil && !allowed(n.Path) {
		return nil, httpapi.NewError(403, "该引用指向容器用量范围之外")
	}
	result["node"] = summary(path)
	result["self_and_omitted_allocated"] = int64(0)
	result["other_entries_allocated"] = int64(0)
	entries := []object{}
	if n != nil {
		result["node"].(object)["allocated"] = n.Allocated
		children := append([]*Node{}, n.Children...)
		slices.SortStableFunc(children, func(a, b *Node) int {
			if a.Allocated > b.Allocated {
				return -1
			}
			if a.Allocated < b.Allocated {
				return 1
			}
			return strings.Compare(a.Path, b.Path)
		})
		visible := []*Node{}
		for _, child := range children {
			if allowed(child.Path) {
				visible = append(visible, child)
			}
		}
		var childBytes, pageBytes int64
		for _, child := range visible {
			childBytes += child.Allocated
		}
		for _, child := range visible[min(offset, len(visible)):min(offset+50, len(visible))] {
			entries = append(entries, summary(child.Path))
			pageBytes += child.Allocated
		}
		result["self_and_omitted_allocated"] = max(int64(0), n.Allocated-childBytes)
		result["other_entries_allocated"] = max(int64(0), childBytes-pageBytes)
		result["has_more"] = offset+50 < len(visible)
	}
	result["entries"], result["offset"] = entries, offset
	return result, nil
}
