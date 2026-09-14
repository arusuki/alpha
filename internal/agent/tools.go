package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
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
		{"get_directory", "从当前记录中按实际占用列出目录明细，包含 Web 和工具已发布的增量探索结果。不扫描；折叠或未记录时会明确提示。", directory},
		{"scan_directory", "在当前记录上增量探索已有物理目录，更新原记录并返回新版本和文件统计；Web 页面同步可见。先读取目录取得 revision，仅在明细不足或需要刷新时调用。", object{"path": str("当前记录中已有的物理目录绝对路径"), "revision": object{"type": "integer", "minimum": 0, "description": "最近一次记录查询返回的 revision；冲突时重新读取"}, "depth": object{"type": "integer", "minimum": 1, "maximum": 32, "description": "本次保留的目录层数，1–32"}}},
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
	agent                              *Manager
	sessionID, userID, actor, recordID string
	detailScans                        int
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
		Revision               int64
		Depth                  int
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, fmt.Errorf("工具参数类型无效")
	}
	if _, ok := spec.Properties["limit"]; ok && (args.Limit < 1 || args.Limit > 50 || args.Offset < 0 || args.Offset > 1000000) {
		return nil, fmt.Errorf("分页 limit 需为 1–50，offset 需为 0–1000000")
	}

	if name == "scan_directory" {
		if args.Revision < 0 || args.Depth < 1 || args.Depth > 32 {
			return nil, fmt.Errorf("记录版本或探索深度无效")
		}
		if t.detailScans >= 6 {
			return nil, fmt.Errorf("本轮已发起 6 次目录探索，请复用结果或在下一轮继续")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		_, err := t.agent.scan(ctx, t.sessionID, t.userID, t.actor, func() (object, error) {
			job, err := t.agent.records.Explore(t.actor, t.recordID, args.Path, args.Revision, args.Depth)
			if err == nil {
				t.detailScans++
			}
			return job, err
		}, false)
		if err != nil {
			return nil, err
		}
		return t.agent.records.Query(t.recordID, "directory", map[string]json.RawMessage{"path": fields["path"]})
	}
	operation := map[string]string{"get_overview": "overview", "list_containers": "containers", "list_owners": "owners", "get_container": "container", "get_directory": "directory"}[name]
	return t.agent.records.Query(t.recordID, operation, fields)
}
