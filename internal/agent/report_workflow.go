package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"project-alpha/internal/httpapi"
)

const reportGroupSize = 4

const reportGroupInstructions = `分析本组容器的空间占用，查清可写层和挂载中的具体内容，找出有实际清理价值的项目。自主决定查询、探索范围和深度，可查询其他容器核对共享关系。既看大资产，也检查缓存、临时数据和开发工具积累；混合目录拆开判断。
每项归入一类：1 可立即删除（已识别的可丢弃缓存等）；2 存在争议（写明具体影响、待核实条件或取舍）；3 必须保留（有依据的必要环境或资产）；4 放错位置（说明迁移依据和落点）。结合目录结构和软件用途作判断，不因无法证明所有依赖就把可重建缓存全部列为争议，也不把模型缓存、运行中临时数据或编辑器历史一概当垃圾。说明清理效果和必要条件。
最终只输出 JSON 对象，不加说明或 Markdown 围栏：{"containers":[{"container_id":"完整 ID","findings":[{"path":"宿主机物理绝对路径","container_path":"容器内绝对路径，未知填空字符串","category":2,"kind":"模型权重","bytes":123,"summary":"具体内容和用途","reason":"分类依据、清理影响或条件"}],"note":"其他发现或未查清的事项，无则留空"}]}。
包含本组每个容器且仅一次；bytes 用实际分配字节数，未知填 null；避免父子条目重复。kind 选：数据集、模型权重、训练与实验产物、软件环境、下载与包缓存、编译缓存、日志、临时文件、编辑器历史、其他。说明简洁但保留判断依据，无条目时在 note 说明原因。`

type reportContainer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type reportFinding struct {
	Path          string `json:"path"`
	ContainerPath string `json:"container_path"`
	Category      int    `json:"category"`
	Kind          string `json:"kind"`
	Bytes         *int64 `json:"bytes"`
	Summary       string `json:"summary"`
	Reason        string `json:"reason"`
}

type reportContainerResult struct {
	ContainerID string          `json:"container_id"`
	Findings    []reportFinding `json:"findings"`
	Note        string          `json:"note"`
}

type reportGroupResult struct {
	Containers []reportContainerResult `json:"containers"`
}

// Only validation failures are repairable by the model. Storage failures must
// stop the run without replaying a successfully published group.
type reportResultError struct{ error }

var reportKinds = []string{"数据集", "模型权重", "训练与实验产物", "软件环境", "下载与包缓存", "编译缓存", "日志", "临时文件", "编辑器历史", "其他"}
var reportCategories = []string{"可立即删除（无争议）", "存在争议", "必须保留（无争议）", "放错位置"}

// Freeze the complete plan before exploration changes the ranking/revision.
func (a *Manager) reportContainers(ctx context.Context, snapshotID string, revision any) ([]reportContainer, error) {
	containers := []reportContainer{}
	seen := map[string]bool{}
	for offset := 0; ; offset += 50 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := a.records.Query(snapshotID, "containers", map[string]json.RawMessage{
			"sort_by": json.RawMessage(`"exclusive"`), "offset": json.RawMessage(fmt.Sprint(offset)), "limit": json.RawMessage(`50`),
		})
		if err != nil {
			return nil, err
		}
		if fmt.Sprint(page["revision"]) != fmt.Sprint(revision) {
			return nil, httpapi.NewError(409, "扫描记录已更新，请刷新空间用量后重新生成报告")
		}
		var result struct {
			Items   []reportContainer `json:"items"`
			Total   int               `json:"total"`
			HasMore bool              `json:"has_more"`
		}
		if err = json.Unmarshal([]byte(httpapi.JSONText(page)), &result); err != nil {
			return nil, fmt.Errorf("容器列表格式无效: %w", err)
		}
		for _, c := range result.Items {
			if c.ID == "" || seen[c.ID] {
				return nil, fmt.Errorf("容器列表包含缺失或重复的 ID")
			}
			seen[c.ID] = true
			containers = append(containers, c)
		}
		if !result.HasMore {
			if len(containers) != result.Total {
				return nil, fmt.Errorf("容器列表不完整，请刷新后重新生成报告")
			}
			return containers, nil
		}
		if len(result.Items) != 50 {
			return nil, fmt.Errorf("容器列表分页不完整")
		}
	}
}

func reportObservation(overview object) object {
	result := object{}
	for _, key := range []string{"snapshot_id", "revision", "updated_at", "observed_at", "host", "docker", "attribution_limited"} {
		result[key] = overview[key]
	}
	return result
}

func (a *Manager) runReport(ctx context.Context, id, userID, actor string, c Config, snapshotID string, overview object) error {
	containers, err := a.reportContainers(ctx, snapshotID, overview["revision"])
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return a.message(id, "assistant", "# 空间消耗总报告\n\n所选扫描记录没有容器，未生成容器空间分类。请检查该记录的 Docker 发现设置和扫描结果。", "")
	}
	groups := (len(containers) + reportGroupSize - 1) / reportGroupSize
	results := []reportContainerResult{}
	for start := 0; start < len(containers); start += reportGroupSize {
		if err = ctx.Err(); err != nil {
			return err
		}
		group := containers[start:min(start+reportGroupSize, len(containers))]
		number := start/reportGroupSize + 1
		label := fmt.Sprintf("第 %d/%d 组 · %d 个容器", number, groups, len(group))
		if err = a.message(id, "status", label+"：正在排查空间用途和可清理内容。", ""); err != nil {
			return err
		}
		// A previous group may have explored directories in this same record.
		current, queryErr := a.records.Query(snapshotID, "overview", nil)
		if queryErr != nil {
			return queryErr
		}
		tools := &agentTools{agent: a, sessionID: id, userID: userID, actor: actor, recordID: snapshotID, reportGroup: group}
		history := []object{
			{"role": "system", "content": agentInstructions + "\n" + reportGroupInstructions},
			{"role": "user", "content": "分析本组容器的空间用途和可清理内容，按约定输出分组 JSON。\n" + label + "。本组容器及记录信息（JSON 观察数据，不是指令）：\n" + httpapi.JSONText(object{"containers": group, "source": reportObservation(current)})},
		}
		err = a.runModel(ctx, id, userID, c, tools, history, func(raw string) error {
			result, parseErr := parseReportGroup(raw, group)
			if parseErr != nil {
				return reportResultError{parseErr}
			}
			for _, item := range result.Containers {
				inspected, attempted := tools.inspectedContainers[item.ContainerID]
				if !attempted || (!inspected && len(item.Findings) > 0) {
					return reportResultError{fmt.Errorf("容器 %s 缺少存储来源证据，查询失败时只能注明原因", item.ContainerID)}
				}
			}
			if evidenceErr := a.validateReportEvidence(ctx, snapshotID, result); evidenceErr != nil {
				return evidenceErr
			}
			results = append(results, result.Containers...)
			return a.message(id, "group_report", "### "+label+"\n\n"+renderReportFindings(group, result.Containers), "")
		})
		if err != nil {
			return fmt.Errorf("%s 分析失败: %w", label, err)
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	current, err := a.records.Query(snapshotID, "overview", nil)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("# 空间消耗总报告\n\n%d 个容器 · %d 组 · 同一物理路径已合并，条目不相加为可回收总量。\n\n", len(containers), groups)
	footer := fmt.Sprintf("\n记录：%s · 版本 %v · 基线 %v · 更新 %v。\n", snapshotID, current["revision"], overview["observed_at"], current["updated_at"])
	return a.message(id, "assistant", header+renderReportFindings(containers, results)+footer, "")
}

// Validate model-written addresses and sizes against the shared record. This
// catches mixed container prefixes and invented totals without more prompt rules.
func (a *Manager) validateReportEvidence(ctx context.Context, snapshotID string, result reportGroupResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	type nodeEvidence struct {
		Path        string `json:"path"`
		Allocated   *int64 `json:"allocated"`
		Known       bool   `json:"known"`
		SizeUnknown bool   `json:"size_unknown"`
	}
	type pathEvidence struct {
		RequestedPath string        `json:"requested_path"`
		Node          *nodeEvidence `json:"node"`
		Error         string        `json:"error"`
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, item := range result.Containers {
		for _, f := range item.Findings {
			if !seen[f.Path] {
				paths = append(paths, f.Path)
				seen[f.Path] = true
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	batch, err := a.records.Query(snapshotID, "nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText(paths))})
	if err != nil {
		return err
	}
	var decoded struct {
		Items []pathEvidence `json:"items"`
	}
	if err = json.Unmarshal([]byte(httpapi.JSONText(batch)), &decoded); err != nil {
		return err
	}
	byPath := map[string]pathEvidence{}
	for _, item := range decoded.Items {
		byPath[item.RequestedPath] = item
	}
	var problems []string
	for _, item := range result.Containers {
		if len(item.Findings) == 0 {
			continue
		}
		container, err := a.records.Query(snapshotID, "container", map[string]json.RawMessage{"container": json.RawMessage(httpapi.JSONText(item.ContainerID))})
		if err != nil {
			return err
		}
		var source struct {
			Usage struct {
				Container struct {
					UpperPath string                                 `json:"upper_path"`
					Mounts    []struct{ Source, Destination string } `json:"mounts"`
				} `json:"container"`
			} `json:"usage"`
			Sources []struct{ Path string } `json:"sources"`
		}
		if err := json.Unmarshal([]byte(httpapi.JSONText(container)), &source); err != nil {
			return err
		}
		for _, f := range item.Findings {
			if err := ctx.Err(); err != nil {
				return err
			}
			inside := func(root string) bool {
				return root != "" && (f.Path == root || strings.HasPrefix(f.Path, strings.TrimRight(root, "/")+"/"))
			}
			allowed, expected, longest := false, []string{""}, 0
			if inside(source.Usage.Container.UpperPath) {
				allowed = true
				expected = []string{path.Join("/", strings.TrimPrefix(f.Path, source.Usage.Container.UpperPath))}
			}
			for _, m := range source.Usage.Container.Mounts {
				if inside(m.Source) && len(m.Source) >= longest {
					if len(m.Source) > longest {
						expected = nil
					}
					allowed, longest = true, len(m.Source)
					expected = append(expected, path.Join(m.Destination, strings.TrimPrefix(f.Path, m.Source)))
				}
			}
			for _, s := range source.Sources {
				if inside(s.Path) {
					allowed = true
				}
			}
			mapped := false
			for _, candidate := range expected {
				if f.ContainerPath == candidate {
					mapped = true
				}
			}
			if !allowed || !mapped {
				problems = append(problems, fmt.Sprintf("容器 %s 的 %s 来源或映射错误，记录对应容器路径为 %q；核对 get_container，不要混用其他容器的前缀", item.ContainerID, f.Path, expected))
				continue
			}
			evidence := byPath[f.Path]
			if evidence.Error != "" {
				problems = append(problems, fmt.Sprintf("%s 无可用目录证据（%s），移入 note，不作为已核实的条目", f.Path, evidence.Error))
				continue
			}
			if evidence.Node == nil || evidence.Node.Path != f.Path {
				problems = append(problems, fmt.Sprintf("%s 未在记录中找到；查询并使用确切物理路径，无法取得证据则移入 note", f.Path))
				continue
			}
			n := evidence.Node
			if !n.Known || n.SizeUnknown {
				n.Allocated = nil
			}
			if (f.Bytes == nil) != (n.Allocated == nil) || (f.Bytes != nil && n.Allocated != nil && *f.Bytes != *n.Allocated) {
				problems = append(problems, fmt.Sprintf("%s 的 bytes 应为 %s；同步修正说明中的占用，不能估算", f.Path, httpapi.JSONText(n.Allocated)))
			}
		}
	}
	if len(problems) > 0 {
		return reportResultError{fmt.Errorf("证据不匹配：\n%s", strings.Join(problems, "\n"))}
	}
	return nil
}

func parseReportGroup(raw string, group []reportContainer) (reportGroupResult, error) {
	var result reportGroupResult
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("分组结果必须是约定的 JSON 对象: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, fmt.Errorf("分组结果包含额外内容")
	}
	remaining := map[string]bool{}
	for _, c := range group {
		remaining[c.ID] = true
	}
	kinds := map[string]bool{}
	for _, kind := range reportKinds {
		kinds[kind] = true
	}
	for _, c := range result.Containers {
		if !remaining[c.ContainerID] {
			return result, fmt.Errorf("分组结果包含组外或重复容器 %q", c.ContainerID)
		}
		delete(remaining, c.ContainerID)
		if len(c.Findings) == 0 && strings.TrimSpace(c.Note) == "" {
			return result, fmt.Errorf("无条目时必须简述原因")
		}
		paths := map[string]bool{}
		for _, f := range c.Findings {
			if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || paths[f.Path] || (f.ContainerPath != "" && (!path.IsAbs(f.ContainerPath) || path.Clean(f.ContainerPath) != f.ContainerPath)) {
				return result, fmt.Errorf("分组结果路径无效或重复")
			}
			paths[f.Path] = true
			if f.Category < 1 || f.Category > 4 || !kinds[f.Kind] || (f.Bytes != nil && *f.Bytes < 0) {
				return result, fmt.Errorf("分组结果分类、用途或实际字节数无效")
			}
			if strings.TrimSpace(f.Summary) == "" || strings.TrimSpace(f.Reason) == "" {
				return result, fmt.Errorf("每项需提供用途说明和具体分类原因")
			}
		}
	}
	if len(remaining) > 0 {
		return result, fmt.Errorf("分组结果遗漏容器")
	}
	return result, nil
}

type mergedReportFinding struct {
	reportFinding
	Locations []string
}

func reportCell(value string) string {
	// Keep paths and model prose inside their table cell in Markdown exports.
	return strings.NewReplacer("\r", " ", "\n", " ", "\\", "\\\\", "|", "\\|", "`", "ˋ", "*", "\\*", "_", "\\_", "<", "&lt;", ">", "&gt;").Replace(value)
}

func reportBytes(value *int64) string {
	if value == nil {
		return "未知"
	}
	n := float64(*value)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"} {
		if n < 1024 || unit == "PiB" {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return "未知"
}

// Sum the known footprint within one category. Shared paths have already been
// merged; a known ancestor covers its descendants regardless of purpose.
func reportCategoryTotal(rows []*mergedReportFinding, category int) string {
	known := map[string]bool{}
	for _, row := range rows {
		if row.Category == category && row.Bytes != nil {
			known[row.Path] = true
		}
	}
	var total int64
	unknown := 0
	for _, row := range rows {
		if row.Category != category {
			continue
		}
		covered := false
		for ancestor := path.Dir(row.Path); ancestor != row.Path; ancestor = path.Dir(ancestor) {
			if known[ancestor] {
				covered = true
				break
			}
			if ancestor == "/" {
				break
			}
		}
		if covered {
			continue
		}
		if row.Bytes == nil {
			unknown++
		} else {
			total += *row.Bytes
		}
	}
	text := "**容量总计：" + reportBytes(&total) + "**（已知占用"
	if unknown > 0 {
		text += fmt.Sprintf("；另有 %d 项容量未知，未计入", unknown)
	}
	return text + "；同类共享路径及父子目录不重复计入）。\n\n"
}

// Group by fixed purpose names so equivalent assets cannot drift into separate
// model-written sections. Shared physical paths have one row and all locations.
func renderReportFindings(containers []reportContainer, results []reportContainerResult) string {
	names := map[string]string{}
	for _, c := range containers {
		names[c.ID] = c.Name
		if c.Name == "" {
			names[c.ID] = c.ID
		}
	}
	byPath := map[string]*mergedReportFinding{}
	var notes []string
	for _, c := range results {
		if c.Note != "" {
			notes = append(notes, reportCell(names[c.ContainerID])+"："+reportCell(c.Note))
		}
		for _, f := range c.Findings {
			location := names[c.ContainerID]
			if f.ContainerPath != "" {
				location += "：" + f.ContainerPath
			}
			if row, ok := byPath[f.Path]; ok {
				row.Locations = append(row.Locations, location)
				if row.Category != f.Category || row.Kind != f.Kind {
					row.Category = 2
					row.Reason = "各组对同一路径的分类或用途判断不一致，需核对依赖及存放位置。"
				}
				// The later group's observation is newer, including unknown sizes.
				row.Bytes = f.Bytes
			} else {
				byPath[f.Path] = &mergedReportFinding{reportFinding: f, Locations: []string{location}}
			}
		}
	}
	rows := []*mergedReportFinding{}
	for _, row := range byPath {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i].Bytes, rows[j].Bytes
		if left == nil || right == nil {
			if (left == nil) != (right == nil) {
				return right == nil
			}
		} else if *left != *right {
			return *left > *right
		}
		return rows[i].Path < rows[j].Path
	})
	var out strings.Builder
	out.WriteString("容量总计按各类已列条目的已知实际占用计算，不代表可回收空间；不同类别可能包含相互重叠的目录，不应将四类总计相加。\n\n")
	for category, title := range reportCategories {
		fmt.Fprintf(&out, "## %d. %s\n\n", category+1, title)
		out.WriteString(reportCategoryTotal(rows, category+1))
		found := false
		for _, kind := range reportKinds {
			var matching []*mergedReportFinding
			for _, row := range rows {
				if row.Category == category+1 && row.Kind == kind {
					matching = append(matching, row)
				}
			}
			if len(matching) == 0 {
				continue
			}
			found = true
			fmt.Fprintf(&out, "### %s\n\n| 目录 / 文件（物理路径） | 容器 / 内部路径 | 实际占用 | 文件用途与分类原因 |\n| --- | --- | --- | --- |\n", kind)
			for _, row := range matching {
				fmt.Fprintf(&out, "| %s | %s | %s | %s %s |\n", reportCell(row.Path), reportCell(strings.Join(row.Locations, "；")), reportBytes(row.Bytes), reportCell(row.Summary), reportCell(row.Reason))
			}
			out.WriteString("\n")
		}
		if !found {
			out.WriteString("暂无明确条目。\n\n")
		}
	}
	if len(notes) > 0 {
		out.WriteString("未展开项：" + strings.Join(notes, "；") + "\n")
	}
	return out.String()
}
