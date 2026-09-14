# 记录读取与增量探索

`storage.Service` 是 Web 和 Agent 共用的业务入口：`Snapshot` 读取公开快照，`Changes` 读取版本差量，`Query` 查询目录和用量，`Explore` 在原记录上启动增量探索。服务不保存模型配置、会话或工具调用历史。

## Web 接口

读取接口要求登录，探索要求管理员权限、JSON 请求体和 `X-CSRF-Token`。

| 接口 | 参数及结果 |
|---|---|
| `GET /api/jobs/:id/snapshot` | 当前已提交的完整记录，含 `revision` |
| `GET /api/jobs/:id/changes?revision=N` | N 之后的目录替换、祖先统计与元数据 |
| `GET /api/jobs/:id/events?revision=N` | 同一差量结果的 SSE 推送 |
| `GET /api/jobs/:id/overview` | 全盘摘要、设备和容器用量、完整性 |
| `GET /api/jobs/:id/containers` | `query`、`sort_by`、`offset`、`limit`；默认按 exclusive 排序 |
| `GET /api/jobs/:id/owners` | `offset`、`limit`；用户去重用量与跨用户共享 |
| `GET /api/jobs/:id/container?container=ID_OR_NAME` | 容器详情与物理目录来源 |
| `GET /api/jobs/:id/directory?path=/srv/data` | `path`、`offset`、`limit`；目录明细和已保存的文件统计 |
| `POST /api/jobs/:id/expand` | `{ "path": "/srv/data", "revision": 0, "depth": 1 }`；返回 202 和目录任务 |

分页默认 `offset=0, limit=30`，最多 50 条。查询响应包含原记录 ID、`revision` 和 `updated_at`，不触发扫描。当前网页的“继续分析”使用同一个 `Explore` 实现；探索更新原记录，结果通过已有轮询和 SSE 显示。

探索只接受记录中已有的物理目录，保留路径排除、单 worker 和乐观版本校验。版本已变化时返回 409，调用方先重新读取；不自动用新版本覆盖用户请求。取消或失败后，已发布目录和版本仍然可读，未完成的文件统计不会当作完整结果保存。

## Agent 接入

应用层把 `storage.Service` 注入 `agent.Records`。模型工具只负责参数形状、工具预算、权限复查和任务等待：查询委托给 `Query`，`scan_directory` 委托给同一个 `Explore`。每次工具查询重新读取记录，因此可以看到用户手动探索的结果；工具探索也立即对 Web 可见。

`directory_analyses` 随快照保存各目录的文件后缀、最大文件和 mtime 统计，包含观察时间；与新探索相交的旧统计失效。扫描只读取文件元数据，统计口径见 [存储统计口径](accounting.md)。
