package agent

import (
	"fmt"

	"project-alpha/internal/httpapi"
)

type reportSource struct {
	SnapshotID string `json:"snapshot_id"`
	Revision   int64  `json:"revision"`
}

// The report contract lives on the server; the browser selects only its source.
const diskReportRequest = `生成空间消耗总报告。基于本次选定的扫描记录，主动完成重点排查并直接给出 Markdown 报告，不要先询问从哪个容器开始。

分析步骤：
1. 整盘对账：读取概览和已有扫描根目录，说明容量、已用、可用、已扫描、容器独占、共享、未关联容器和未解释空间。区分扫描范围与整台主机；如果未覆盖 / 或 Docker 发现不完整，不得声称覆盖全盘。
2. 定位大头：分别按独占实际占用和可写层实际占用查容器排行，结合用户归属选出优先排查对象，说明选择依据。读取大容器的 get_container 和 get_directory，也检查共享挂载和宿主机大目录。超过分页上限继续分页或明确只覆盖前 N 项。
3. 路径映射：分开梳理 bind mount、volume、可写层、容器日志和宿主机数据；用宿主机物理路径 → 容器内绝对路径说明数据落点。可写层相对 upper_path 映射到容器 /，挂载由 source 映射到 destination；嵌套挂载按最长路径匹配。不要写含糊的 ~，没有证据不能假定 HOME=/root。不要把 overlay2 整体当作可写层总和，也不要对 merged 视图再计数。
4. 大目录分类：按占用降序整理数据集、模型权重、训练/评测产物、软件环境、可重建缓存、日志/临时文件、编辑器历史/检查点和未知内容。路径名和扩展名只是分类线索；Hugging Face/ModelScope 缓存中的数据集和模型仍是数据资产，不能一概视为垃圾。关注 pip/uv/conda、编译缓存、VS Code/Cline 历史、Ray 日志等模式，但只有当前记录存在证据时才写具体发现，禁止套用其他机器的路径、容器或数值。
5. 按需补查：优先复用已有明细，将最多 6 次目录探索分配给最值得解释的大头，每次深度 1–32；需要文件类型和日期证据时可 scan_directory 收集元数据。保留足够模型轮次完成报告。时间分类使用明确的 90 天以内、90–180 天、180 天及以上 mtime 分桶，组内按实际占用降序；以 analysis_observed_at 为观察时刻。目录跨时间桶时分列已观测文件量，不能把整个目录硬塞进一个桶。缺少日期的项单列“时间未知”；最大 40 个文件仅是样本，不能冒充全量数据集清单。
6. 总结前重新读取 get_overview，标记记录 ID、最终 revision、基线扫描时间及局部补查时间。对账数字采用同一最新记录，不把父子目录、共享引用、候选子集重复相加。

最终报告按以下结构输出，各节无证据时写清原因：
# 空间消耗总报告
## 总览与主要结论
## 重点容器与用户排行
## 挂载、可写层与宿主机占用
## 大数据资产与时间分布
## 清理候选与长期治理
## 覆盖范围、证据与待确认事项

表格尽量包含：优先级、容器/负责人、存储来源、容器内绝对路径、宿主机路径、实际占用（易读单位并附字节数）、内容类别/证据、时间依据、建议与待确认条件。表格内路径如含竖线需转义。
清理建议区分“可重建候选”“数据资产需负责人确认”“证据不足暂缓”；说明删除历史/检查点会失去回滚或排查能力，模型缓存可能仍被任务依赖。候选占用不是已确认可释放空间，无法去重时不报可回收总量。进程、打开文件、内容哈希和真实创建/下载时间均未验证，不得声称安全删除或数据重复；列出负责人需要核实的事项。建议可包括日志轮转、缓存限额、数据/模型共享挂载和迁出可写层，但不执行清理。`

func (a *Manager) validateReportSource(source *reportSource) error {
	job, err := a.records.Job(source.SnapshotID)
	if err != nil {
		return err
	}
	if job["status"] != "completed" || job["trigger"] == "incremental" {
		return httpapi.NewError(409, "请选择已完成的扫描记录生成报告")
	}
	if fmt.Sprint(job["snapshot_revision"]) != fmt.Sprint(source.Revision) {
		return httpapi.NewError(409, "扫描记录已更新，请刷新空间用量后重新生成报告")
	}
	return nil
}
