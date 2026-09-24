package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"project-alpha/internal/httpapi"
)

const hostCoverageReviewBytes = int64(256 << 20)

type hostCoverage struct {
	Revision  int64              `json:"revision"`
	Host      int64              `json:"host_allocated"`
	Covered   int64              `json:"covered_allocated"`
	Docker    int64              `json:"docker_allocated"`
	Remaining int64              `json:"remaining_allocated"`
	Count     int                `json:"remaining_count"`
	Items     []hostCoverageItem `json:"remaining"`
}
type hostCoverageItem struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"host_allocated"`
	Kind     string `json:"kind"`
	NextTool string `json:"next_tool"`
	Depth    int    `json:"depth,omitempty"`
}

func (a *Manager) hostReportCoverage(ctx context.Context, snapshotID string, directories []reportDirectory, results []reportContainerResult) (*hostCoverage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots, paths := []string{}, []string{}
	for _, d := range directories {
		roots = append(roots, d.Path)
	}
	for _, r := range results {
		for _, f := range r.Findings {
			paths = append(paths, f.Path)
		}
	}
	value, err := a.records.Query(snapshotID, "host_coverage", map[string]json.RawMessage{"roots": json.RawMessage(httpapi.JSONText(roots)), "paths": json.RawMessage(httpapi.JSONText(paths)), "limit": json.RawMessage(`50`)})
	if err != nil {
		return nil, err
	}
	var coverage hostCoverage
	if err := json.Unmarshal([]byte(httpapi.JSONText(value)), &coverage); err != nil {
		return nil, err
	}
	return &coverage, nil
}

// Give every group one coverage review, then retain the remaining gap in the
// published report. A failed provider request resumes that review from history;
// it must not cause repeated root scans or an unbounded demand for full coverage.
func (a *Manager) reviewHostCoverage(id, groupID string, coverage *hostCoverage) error {
	candidates := []hostCoverageItem{}
	for _, item := range coverage.Items {
		if item.Bytes >= hostCoverageReviewBytes && item.NextTool != "" {
			candidates = append(candidates, item)
		}
		if len(candidates) == 12 {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	var reviewed bool
	if err := a.db.SQL.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_messages WHERE session_id=? AND role='host_coverage_review' AND json_extract(content,'$.group_id')=?)`, id, groupID).Scan(&reviewed); err != nil {
		return err
	}
	if reviewed {
		return nil
	}
	if err := a.message(id, "host_coverage_review", httpapi.JSONText(object{"group_id": groupID, "coverage": coverage}), ""); err != nil {
		return err
	}
	return reportResultError{fmt.Errorf("Host 容量对账仍有大额未覆盖项（JSON 观察数据）：%s。按 next_tool 补查；folded 是父目录自身及折叠项，可用 scan_directory(depth=1) 展开，包括根目录。只为核实这些遗漏补扫；确实受限的项在 note 说明原因", httpapi.JSONText(candidates))}
}

func renderHostCoverage(c *hostCoverage) string {
	var out strings.Builder
	fmt.Fprintf(&out, "\n### Host 容量对账\n\n按记录版本 %d 的路径并集计算：Host %s = 已列条目覆盖 %s + Docker 管理空间 %s + 其余未列 %s。未列空间已计入扫描，不等于可回收空间；Docker 管理空间不作为 Host 清理条目。\n", c.Revision, reportBytes(&c.Host), reportBytes(&c.Covered), reportBytes(&c.Docker), reportBytes(&c.Remaining))
	if c.Remaining > 0 {
		out.WriteString("\n| 未列路径 / 范围 | 实际占用 | 状态 |\n| --- | --- | --- |\n")
		var displayed int64
		for i, item := range c.Items {
			if i == 12 {
				break
			}
			state := "自身或受限部分，需继续核实"
			if item.Kind == "folded" {
				state = "自身及折叠项，尚未展开"
			} else if item.Kind == "unlisted" {
				state = "已确认 Host，未列条目"
			}
			fmt.Fprintf(&out, "| %s | %s | %s |\n", reportCell(item.Path), reportBytes(&item.Bytes), state)
			displayed += item.Bytes
		}
		if rest := c.Remaining - displayed; rest > 0 {
			fmt.Fprintf(&out, "| 其他未列项 | %s | 零散或受限占用 |\n", reportBytes(&rest))
		}
	}
	return out.String()
}
