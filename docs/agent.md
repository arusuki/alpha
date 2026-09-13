# Agent API

管理员在“设置 → API Key 与模型”配置 Chat Completions 或 Responses 连接，分析通过 API 发起。新建分析先扫描宿主机 `/`，启用 Docker 自动发现时也扫描 Docker 资源；追问复用该会话的全盘快照。模型可读取统计并发起只读目录扫描。

## HTTP 接口

所有接口要求管理员会话。变更请求携带 `X-CSRF-Token` 和 JSON 请求体，令牌从 `GET /api/session` 获取；分析会话仅创建者可访问。

| 接口 | 请求与结果 |
|---|---|
| `GET /api/agent/settings` | 配置及版本，Key 仅返回 `has_api_key` |
| `PUT /api/agent/settings` | `{revision, value}`；value 包含 `protocol`、`endpoint`、`model`、`api_key`、`clear_api_key`、`max_rounds`、`timeout_seconds` |
| `GET /api/agent/sessions` | 当前账号最近 50 个分析 |
| `POST /api/agent/sessions` | `{message}`；新建分析，返回 202 |
| `GET /api/agent/sessions/:id?after=0` | 状态、扫描进度及消息；最多 200 条，返回 `next_after`、`has_more` |
| `POST /api/agent/sessions/:id/messages` | `{message}`；继续提问 |
| `POST /api/agent/sessions/:id/cancel` | `{}`；停止模型请求及本轮扫描 |

`protocol` 为 `completions` 或 `responses`，`endpoint` 支持基础地址或完整接口地址。保存空 Key 表示保留，`clear_api_key:true` 表示清除。

## 模型工具

| 工具 | 参数 | 用途 |
|---|---|---|
| `get_overview` | `{}` | 全盘摘要、设备对账及完整性 |
| `list_containers` | `query, sort_by, offset, limit` | 容器排行；支持 `exclusive`、`writable`、`docker_logical` 排序 |
| `list_owners` | `offset, limit` | 用户去重用量与跨用户共享 |
| `get_container` | `container` | 按完整 ID 或精确名称查询存储来源 |
| `get_directory` | `path, offset, limit` | 查询已保存的目录明细 |
| `scan_directory` | `path, refresh` | 局部扫描；缓存 10 分钟，`refresh:true` 强制更新 |

分页最多 50 项。路径须为会话扫描范围内的宿主机物理绝对路径；排除平台数据、虚拟文件系统及 Docker merged 视图。局部扫描复用基线的后端和排除设置，结果不替换首页全盘快照。

## 执行与数据

同一服务同时运行一个分析，扫描共用单 worker。单次分析最长 2 小时，单次目录细查最多 10 分钟，每轮最多 6 次实际细查。服务关闭会取消分析，异常重启后标为中断；已保存的消息和结果保留。

目录细查返回最大 40 个文件、扩展名汇总及 90/180 天 mtime 分布，只读取元数据。mtime 和 ctime 不能证明文件最近被读取或可以删除；全盘与局部观察不能相加。统计定义见 [存储统计口径](accounting.md)。

模型服务会收到问题、路径和统计结果。API Key 保存在权限为 `0600` 的 SQLite 文件中，不额外加密，完整备份包含凭据。Key 不回传浏览器、不写入审计或扫描快照；历史消息不保存原始 reasoning items。
