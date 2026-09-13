package storage

import (
	"fmt"
	"path/filepath"
)

// Allocated and Apparent use the physical tree's global inode deduplication.
// They are observations already included in the tree, not extra disk usage.
// Nil distinguishes an unreadable layer from a successfully scanned empty one.
type WritableLayer struct {
	Allocated        *int64 `json:"allocated"`
	Apparent         *int64 `json:"apparent"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	PermissionDenied bool   `json:"permission_denied"`
}

func writableLayers(containers []Container, tree *Node) (object, []Warning) {
	nodes := map[string]*Node{}
	references := []*Node{}
	pending := []*Node{tree}
	for len(pending) > 0 {
		n := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		nodes[n.Path] = n
		if n.Kind == "reference" {
			references = append(references, n)
		}
		pending = append(pending, n.Children...)
	}
	deferred := map[string]bool{}
	for path, n := range nodes {
		if n.Scanning || n.SizeUnknown {
			for p := path; ; p = filepath.Dir(p) {
				deferred[p] = true
				if p == filepath.Dir(p) {
					break
				}
			}
		}
	}
	complete, partial, unknown, denied := 0, 0, 0, 0
	warnings := []Warning{}
	for i := range containers {
		c := &containers[i]
		w := &WritableLayer{Status: "unavailable", Reason: "存储驱动未提供可写层 UpperDir，无法统计实际分配空间"}
		c.WritableLayer = w
		if c.UpperPath != nil {
			externalReference := false
			for _, ref := range references {
				if within(ref.Path, *c.UpperPath) && !within(ref.Reference, *c.UpperPath) {
					externalReference = true
					break
				}
			}
			n := nodes[*c.UpperPath]
			if n == nil {
				w.Reason = "可写层未被遍历；请检查扫描范围、排除目录和父目录读取权限"
				for p := filepath.Dir(*c.UpperPath); ; p = filepath.Dir(p) {
					if ancestor := nodes[p]; ancestor != nil && (ancestor.Reason != "" || ancestor.Kind == "excluded") {
						n = ancestor
						break
					}
					if p == filepath.Dir(p) {
						break
					}
				}
			}
			if n != nil {
				w.PermissionDenied = n.PermissionDenied > 0
				switch {
				case n.Kind == "excluded":
					w.Status, w.Reason = "excluded", "可写层或其父目录被排除，未统计实际分配空间"
				case n.Kind == "unreadable" || (n.Reason != "" && (n.Path != *c.UpperPath || (n.Files == 0 && len(n.Children) == 0))):
					w.Status, w.Reason = "unreadable", fmt.Sprintf("无法完整读取 %s：%s", n.Path, n.Reason)
				default:
					w.Status, w.Reason = "complete", ""
					allocated, apparent := n.Allocated, n.Apparent
					w.Allocated, w.Apparent = &allocated, &apparent
					if n.Errors > 0 || n.Excluded > 0 || n.OmittedReferences > 0 || externalReference || deferred[n.Path] {
						w.Status = "partial"
						w.Reason = "仅列出已计入扫描的去重空间；存在读取异常、排除项或共享引用，不能视为完整的可写层用量"
						if deferred[n.Path] {
							w.Reason = "按层分析中；未读取的深层内容沿用历史统计，缺少明细的目录待继续分析"
						}
					}
				}
				if w.PermissionDenied {
					w.Reason = "可写层存在权限不足；请使用 Docker 只读辅助扫描，或具备相应目录读取权限的宿主机扫描服务后重新扫描"
				}
			}
		}
		switch w.Status {
		case "complete":
			complete++
		case "partial":
			partial++
		default:
			unknown++
		}
		if w.PermissionDenied {
			denied++
		}
		if w.Status != "complete" {
			warnings = append(warnings, Warning{c.Name, "可写层：" + w.Reason})
		}
	}
	return object{"total": len(containers), "complete": complete, "partial": partial, "unknown": unknown, "permission_denied": denied}, warnings
}
