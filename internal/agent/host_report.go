package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"project-alpha/internal/httpapi"
)

const hostReportInstructions = `分析本组 Host 目录中未被容器引用的空间，查清具体用途和可清理内容。使用 get_host_directory 查看按容器归属扣除后的 Host 实际占用，按需使用 scan_directory 补查折叠目录；混合目录须继续下钻，只把 host_only 为 true 的具体物理路径列为条目。attribution_limited 是全盘汇总警告，以具体路径的 host_only 判断能否列入；已有明细先查询，只补扫需要分析的折叠子目录，不为消除全盘警告重扫根目录。不要把容器可写层、挂载源或包含它们的父目录写入 Host 报告。未知或归属不完整的路径只能在 note 说明，不能猜测占用。
普通明细折叠、已核实的 Host 路径之间的硬链接不影响 Host 归属。host_only=false 时按 host_only_blocker 的具体路径和原因解释；权限不足、扫描错误不等于容器共享，不根据零容器字节猜测原因。Host 归属不代表可以删除，硬链接的已计占用也不等于删除后释放空间。
每项归入一类：1 可立即删除；2 存在争议；3 必须保留；4 放错位置。说明用途、分类依据和清理影响。kind 选：数据集、模型权重、训练与实验产物、软件环境、下载与包缓存、编译缓存、日志、临时文件、编辑器历史、其他。
最终只输出 JSON 对象，不加说明或 Markdown 围栏：{"directories":[{"path":"本组分配的一级物理目录","findings":[{"path":"宿主机物理绝对路径","category":2,"kind":"日志","bytes":123,"summary":"具体内容和用途","reason":"分类依据、清理影响或条件"}],"note":"未查清的事项，无则留空"}]}。
包含本组每个目录且仅一次；bytes 用该路径的实际分配字节数，未知填 null；避免父子条目重复。无条目时在 note 说明原因。发布前按容量对账反馈补查大额遗漏和折叠项，确实受限时说明原因。`

type reportDirectory struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type hostReportFinding struct {
	Path     string `json:"path"`
	Category int    `json:"category"`
	Kind     string `json:"kind"`
	Bytes    *int64 `json:"bytes"`
	Summary  string `json:"summary"`
	Reason   string `json:"reason"`
}

type hostReportDirectoryResult struct {
	Path     string              `json:"path"`
	Findings []hostReportFinding `json:"findings"`
	Note     string              `json:"note"`
}

type hostReportGroupResult struct {
	Directories []hostReportDirectoryResult `json:"directories"`
}

func parseHostReportGroup(raw string, group []reportDirectory) (hostReportGroupResult, error) {
	var result hostReportGroupResult
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("Host 分组结果必须是约定的 JSON 对象: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || result.Directories == nil {
		return result, fmt.Errorf("Host 分组结果必须包含 directories 数组，且不得包含额外输出")
	}
	remaining := map[string]bool{}
	for _, directory := range group {
		remaining[directory.Path] = true
	}
	kinds := map[string]bool{}
	for _, kind := range reportKinds {
		kinds[kind] = true
	}
	for _, directory := range result.Directories {
		if !remaining[directory.Path] {
			return result, fmt.Errorf("Host 分组结果包含组外或重复目录 %q", directory.Path)
		}
		delete(remaining, directory.Path)
		if len(directory.Findings) == 0 && strings.TrimSpace(directory.Note) == "" {
			return result, fmt.Errorf("Host 目录 %s 无条目时必须简述原因", directory.Path)
		}
		seen := map[string]bool{}
		for _, finding := range directory.Findings {
			if !path.IsAbs(finding.Path) || path.Clean(finding.Path) != finding.Path || seen[finding.Path] || !withinReportRoot(finding.Path, directory.Path) {
				return result, fmt.Errorf("Host 分组结果路径无效、重复或不属于分配目录")
			}
			seen[finding.Path] = true
			if finding.Category < 1 || finding.Category > 4 || !kinds[finding.Kind] || (finding.Bytes != nil && *finding.Bytes < 0) {
				return result, fmt.Errorf("Host 分组结果分类、用途或实际字节数无效")
			}
			if strings.TrimSpace(finding.Summary) == "" || strings.TrimSpace(finding.Reason) == "" {
				return result, fmt.Errorf("Host 每项需提供用途说明和具体分类原因")
			}
		}
	}
	if len(remaining) > 0 {
		return result, fmt.Errorf("Host 分组结果遗漏目录")
	}
	return result, nil
}

func withinReportRoot(p, root string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

func hostReportSubjects(directories []reportDirectory) []reportSubject {
	result := make([]reportSubject, 0, len(directories))
	for _, directory := range directories {
		name := directory.Name
		if name == "" {
			name = directory.Path
		}
		result = append(result, reportSubject{ID: directory.Path, Name: name})
	}
	return result
}

func hostReportResults(results []hostReportDirectoryResult) []reportResult {
	converted := make([]reportResult, 0, len(results))
	for _, directory := range results {
		item := reportResult{SubjectID: directory.Path, Note: directory.Note}
		for _, f := range directory.Findings {
			item.Findings = append(item.Findings, reportFinding{Path: f.Path, Category: f.Category, Kind: f.Kind, Bytes: f.Bytes, Summary: f.Summary, Reason: f.Reason})
		}
		converted = append(converted, item)
	}
	return converted
}

func (a *Manager) hostReportRoots(ctx context.Context, snapshotID string, revision any) ([]reportDirectory, error) {
	directories := []reportDirectory{}
	seen := map[string]bool{}
	for offset := 0; ; offset += 50 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := a.records.Query(snapshotID, "host_roots", map[string]json.RawMessage{"offset": json.RawMessage(fmt.Sprint(offset)), "limit": json.RawMessage(`50`)})
		if err != nil {
			return nil, err
		}
		if fmt.Sprint(page["revision"]) != fmt.Sprint(revision) {
			return nil, httpapi.NewError(409, "扫描记录已更新，请刷新空间用量后重新生成报告")
		}
		var result struct {
			Items   []reportDirectory `json:"items"`
			Total   int               `json:"total"`
			HasMore bool              `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(httpapi.JSONText(page)), &result); err != nil {
			return nil, fmt.Errorf("Host 目录列表格式无效: %w", err)
		}
		for _, directory := range result.Items {
			if !path.IsAbs(directory.Path) || path.Clean(directory.Path) != directory.Path || seen[directory.Path] {
				return nil, fmt.Errorf("Host 目录列表包含无效或重复路径")
			}
			seen[directory.Path] = true
			directories = append(directories, directory)
		}
		if !result.HasMore {
			if len(directories) != result.Total {
				return nil, fmt.Errorf("Host 目录列表不完整，请刷新后重新生成报告")
			}
			return directories, nil
		}
		if len(result.Items) != 50 {
			return nil, fmt.Errorf("Host 目录列表分页不完整")
		}
	}
}

func (a *Manager) runHostReport(ctx context.Context, id, userID, actor string, c Config, snapshotID string, overview object, concurrency int) error {
	directories, err := a.hostReportRoots(ctx, snapshotID, overview["revision"])
	if err != nil {
		return err
	}
	if len(directories) == 0 {
		return a.saveReport(id, "# Host 空间分析报告\n\n所选扫描记录没有可分析的 Host 目录。请检查扫描配置中的 Host 根目录和扫描结果。", nil, nil)
	}
	groups := []savedReportGroup{}
	for start := 0; start < len(directories); start += reportGroupSize {
		number := len(groups) + 1
		groups = append(groups, savedReportGroup{ID: fmt.Sprintf("group-%d", number), Number: number, Directories: directories[start:min(start+reportGroupSize, len(directories))]})
	}
	return a.runReportPlan(ctx, id, userID, actor, c, snapshotID, "host", groups, concurrency)
}

func (a *Manager) validateHostReportEvidence(ctx context.Context, snapshotID string, result hostReportGroupResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	type hostNode struct {
		Path        string `json:"path"`
		Kind        string `json:"kind"`
		Allocated   *int64 `json:"allocated"`
		Known       bool   `json:"known"`
		SizeUnknown bool   `json:"size_unknown"`
		HostOnly    bool   `json:"host_only"`
		Blocker     *struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"host_only_blocker"`
	}
	type pathEvidence struct {
		RequestedPath string    `json:"requested_path"`
		Node          *hostNode `json:"node"`
		Error         string    `json:"error"`
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, directory := range result.Directories {
		for _, finding := range directory.Findings {
			if !seen[finding.Path] {
				paths = append(paths, finding.Path)
				seen[finding.Path] = true
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	page, err := a.records.Query(snapshotID, "host_nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText(paths))})
	if err != nil {
		return err
	}
	var decoded struct {
		Items []pathEvidence `json:"items"`
	}
	if err := json.Unmarshal([]byte(httpapi.JSONText(page)), &decoded); err != nil {
		return err
	}
	byPath := map[string]pathEvidence{}
	for _, item := range decoded.Items {
		byPath[item.RequestedPath] = item
	}
	var problems []string
	for _, directory := range result.Directories {
		for _, finding := range directory.Findings {
			if err := ctx.Err(); err != nil {
				return err
			}
			evidence, ok := byPath[finding.Path]
			if !ok || evidence.Error != "" || evidence.Node == nil || evidence.Node.Path != finding.Path {
				problems = append(problems, fmt.Sprintf("%s 无可用的 Host 目录证据；请查询确切路径，无法核实时移入 note", finding.Path))
				continue
			}
			node := evidence.Node
			if node.Kind != "file" && node.Kind != "directory" {
				problems = append(problems, fmt.Sprintf("%s 必须是具体文件或目录，不能直接列出链接或引用", finding.Path))
				continue
			}
			if !node.HostOnly {
				reason := "未取得完整的 Host 路径证据"
				if node.Blocker != nil {
					reason = node.Blocker.Path + "：" + node.Blocker.Message
				}
				problems = append(problems, fmt.Sprintf("%s 不能作为 Host 条目（%s）；继续下钻到 host_only 路径", finding.Path, reason))
				continue
			}
			if !node.Known || node.SizeUnknown {
				problems = append(problems, fmt.Sprintf("%s 容量或目录状态未知，不能作为已核实的 Host 条目", finding.Path))
				continue
			}
			if (finding.Bytes == nil) != (node.Allocated == nil) || (finding.Bytes != nil && node.Allocated != nil && *finding.Bytes != *node.Allocated) {
				problems = append(problems, fmt.Sprintf("%s 的 bytes 应为 %s；不能估算", finding.Path, httpapi.JSONText(node.Allocated)))
			}
		}
	}
	if len(problems) > 0 {
		return reportResultError{fmt.Errorf("Host 证据不匹配：\n%s", strings.Join(problems, "\n"))}
	}
	return nil
}
