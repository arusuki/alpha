# 记录读取与增量探索

`storage.Service` 是 Web 和 Agent 共用的业务入口：`Snapshot` 读取公开快照，`Changes` 读取版本差量，`Query` 查询目录和用量，`Explore` 在原记录上启动增量探索。服务不保存模型配置、会话或工具调用历史。

Docker 扫描保存本机 endpoint、daemon ID 和扫描时的物理数据根，供 Host 清理前复核。格式版本与不兼容数据的处理见 [运行与配置](operations.md#配置与数据)。

## 自动扫盘与记录保留

管理员在总控的 worker 节点卡片选择 **扫盘计划**，或进入节点的 **扫描配置**，为各节点独立保存计划：

- **关闭自动扫描**：只手动触发。
- **按间隔扫描**：设置 5–10080 分钟，从上次全盘任务结束后计时。没有历史记录时立即开始。
- **按时间计划扫描**：选择星期、一个或多个 `HH:MM` 时刻以及 IANA 时区（例如 `Asia/Shanghai`）。选满七天即每天执行。最多 24 个时刻，按分钟触发；节点离线或整个目标分钟都有扫描执行时跳过，不累计补跑。夏令时不存在的时刻跳过，重复时刻在两个实际分钟分别执行。
- **滚动保留记录数**：`0` 关闭清理；`N` 保留按创建时间排序的最近 N 条已结束全盘记录，包括手动、自动、失败和取消的任务。目录增量任务随所属全盘记录清理，不单独计数。

计划保存在 worker，由已有扫描管理循环执行，总控断开不影响计划。每个节点仍只运行一个扫描。保留策略在节点空闲时生效，删除超出的数据库记录、关联目录任务和结果文件，保留宿主机源文件；即使关闭自动扫描，已设置的保留策略仍然生效。正在执行的任务不被清理。

设置通过节点代理 `GET/PUT /api/cluster/nodes/:id/api/settings` 读取和保存，写入要求管理员权限、CSRF 和当前 `revision`。新增字段为 `schedule_mode`（`off/interval/calendar`）、`schedule_times`、`schedule_weekdays`（0 为周日）、`schedule_timezone`、`retain_records`，间隔仍使用 `interval_minutes`。`GET .../api/state` 提供计划摘要和历史记录边界，页面自动移除过期记录。

数据库升级沿用 [alpha-updater](updater.md) 的原地升级入口与生成窗口（`internal/platform/upgrade_history.json`），目标版本以 `alpha-updater --help` 为准。升级保留已有扫描间隔、记录及快照，默认不开启自动清理；总控和 worker 需一起更新到相同集群协议。

## Web 接口

读取接口要求登录，探索要求管理员权限、JSON 请求体和 `X-CSRF-Token`。

| 接口 | 参数及结果 |
|---|---|
| `GET /api/jobs/:id/view?path=/srv/data` | 服务端计算的全局用量、来源摘要和当前目录一层明细；省略 path 时打开扫描根 |
| `GET /api/jobs/:id/snapshot` | 当前已提交的完整记录，含 `revision` |
| `GET /api/jobs/:id/changes?revision=N` | N 之后的目录替换、祖先统计与元数据 |
| `GET /api/jobs/:id/events?revision=N` | 同一差量结果的 SSE 推送；`view=1` 时仅通知最新版本 |
| `GET /api/jobs/:id/overview` | 全盘摘要、设备和容器用量、完整性 |
| `GET /api/jobs/:id/containers` | `query`、`sort_by`、`offset`、`limit`；默认按 exclusive 排序 |
| `GET /api/jobs/:id/owners` | `offset`、`limit`；用户去重用量与跨用户共享 |
| `GET /api/jobs/:id/container?container=ID_OR_NAME` | 容器详情与物理目录来源 |
| `GET /api/jobs/:id/directory?path=/srv/data` | `path`、`offset`、`limit`；目录明细和已保存的文件统计 |
| `POST /api/jobs/:id/expand` | `{ "path": "/srv/data", "revision": 0, "depth": 8 }`；返回 202 和目录任务 |

网页使用 `view` 按需读取展示数据，不下载或本地缓存完整快照，也不在浏览器解析全树、计算归属。目录下钻只读取当前层；SSE 通知后重新读取当前视图，保留选择、路径和搜索条件。后端每个 Handler 最多缓存一份已解析记录和统计索引，目录版本、归属或基线文件变化时失效，读取缓存仍校验登录和记录存在。

分页默认 `offset=0, limit=30`，最多 50 条。查询响应包含原记录 ID、`revision` 和 `updated_at`，不触发扫描。当前网页的“继续分析”使用同一个 `Explore` 实现；探索更新原记录，结果通过已有轮询和 SSE 显示。

`depth` 可选 1–32，省略时为 1。网页默认一次探索 3 层，并保留本页面选择的深度供后续探索使用。每次递归只遍历目标子树一次，字节统计与文件元数据分析共用这次遍历；深度控制保留的明细层数，不缩小统计范围。已记录的多层明细可直接查询，无需再次扫描。

探索只接受记录中已有的物理目录，保留路径排除、单 worker 和乐观版本校验。版本已变化时返回 409，调用方先重新读取；不自动用新版本覆盖用户请求。取消或失败后，已发布目录和版本仍然可读，未完成的文件统计不会当作完整结果保存。

目录发布在一个 worker 内复用固定基线与已提交的节点索引，只处理目标分支和祖先，跳过不变子树及不变行。节点比较与 JSON 编码在写事务外完成；大批节点最多用 4 个编码协程，每批 128 行写入，元数据、节点、版本和完成状态仍在同一事务提交。缓存仅在提交成功后推进，失败可使用同一发布器重试。

扫描与合并保存并行；扫描器至少间隔 500 毫秒生成一次目录观察，待处理观察和进度各只保留最新一份。完整结果到达后跳过它已覆盖的排队观察；首次观察仍发布，最终结果同步提交。回调失败会取消扫描并等待扫描协程退出。Docker 辅助扫描的目录更新直接解码为节点，避免通用 JSON 对象转换。

接单时通过索引检查已有记录的目标节点与版本，完整快照由 worker 加载。首次探索尚未写入节点表的基线时，接单校验仍需读取基线文件。缓存不替代下一次探索的文件系统测量。

## Agent 接入

应用层把 `storage.Service` 注入 `agent.Records`。模型工具只负责参数形状、工具预算、权限复查和任务等待：查询委托给 `Query`，`scan_directory` 委托给同一个 `Explore`。每次工具查询重新读取记录，因此可以看到用户手动探索的结果；工具探索也立即对 Web 可见。

Host 查询包括 `host_roots`、`host_directory` 和 `host_nodes`：只规划显式配置的 Host 扫描根，返回 `host_allocated`、`container_allocated`、`host_only`，并以 `host_only_blocker` 说明无法核实归属的路径和原因。报告只接受归属完整的物理路径；具体判定见 [存储统计口径](accounting.md)。

`omitted_reference_targets` 保存折叠引用的目标证据。局部扫描保留外部引用约束、未核实的旧引用及容器资源边界；全盘 `attribution_limited` 警告不会直接否决无关的 Host 路径。

`directory_analyses` 随快照保存各目录的文件后缀、最大文件和 mtime 统计，包含观察时间；与新探索相交的旧统计失效。扫描只读取文件元数据，统计口径见 [存储统计口径](accounting.md)。

`host_coverage` 是只读对账查询，接受 `roots`、已列物理路径 `paths` 及分页参数。按路径并集计量，返回 `host_allocated = covered_allocated + docker_allocated + remaining_allocated`；`remaining` 按容量排序，区分 `unlisted`（可列入的 Host 路径）、`folded`（父目录自身及折叠项）和 `residual`（其他剩余）。混合目录继续下钻找 Host 子项，Docker 管理目录单列且不建议补扫；分页不会改变全量合计。查询不触发扫描。
