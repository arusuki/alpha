# project alpha

单宿主机管理平台，登录后从总面板进入存储、进程管理或 Agent 设置。自动发现 Docker 容器，按容器和用户汇总存储占用，支持目录下钻、按需扫描、任务管理和 Agent 空间报告。后端使用 Go 和 SQLite，网页资源嵌入二进制。

## 代码结构

- `cmd/project-alpha`：程序入口，处理进程信号并启动应用。
- `internal/app`：命令分发、模块装配和 HTTP 服务生命周期。
- `internal/platform`：平台 HTTP 入口、账号与会话、访问校验、审计和数据库基础；不依赖存储模块。
- `internal/storage`：扫描配置、Docker 发现与辅助扫描、任务调度、快照与增量更新、共享记录读取与探索服务，以及对应 API 和数据表。
- `internal/agent`：模型配置与凭据、Responses/Chat Completions 适配、分析会话、工具定义与编排，以及独立 API 和数据表。
- `internal/process`：订阅 Tetragon 进程事件，常驻维护并按容器导出活动进程森林。
- `internal/httpapi`、`internal/fsutil`：共用的 HTTP/JSON 处理与路径规范化。
- `dist`：网页资源及 Go 嵌入声明；`tests`：前端回归和共享测试数据。Go 测试与所属包放在一起。

应用层装配平台、存储和 Agent 模块；平台完成会话与请求校验后，通过 `platform.Module` 分发业务请求。各模块的数据表由各自维护的 `schema.sql` 定义，在同一事务中初始化。Agent 通过 `agent.Records` 接口使用应用层注入的 `storage.Service`；两个业务包互不导入。Agent 生命周期和完成状态重试独立于扫描管理器。

Web 的快照、变更读取和目录探索，以及 Agent 的查询工具，共用 `storage.Service`。探索使用原记录 ID、`revision` 和 1–32 层 `depth`，发布到同一条记录；两边都能读到最新已提交版本。目录文件统计也随记录持久化，不维护 Agent 私有的目录快照或缓存。详见 [记录读取与探索](docs/records.md)。

## 启动

要求 Linux、Go 1.26+ 和 GCC；Docker 扫描需要本机 Docker CLI 及 daemon 访问权限。

```bash
go build -o bin/project-alpha ./cmd/project-alpha
./bin/project-alpha
```

打开 <http://127.0.0.1:8765> 创建管理员，在存储模块的“扫描配置”中配置目录并开始扫描。只读账号可以查看整台主机的扫描结果。

管理员配置“Agent 设置 → API Key 与模型”后，可在空间用量页点击“生成空间报告”。Agent 基于当前扫描记录排查重点容器、挂载与可写层，分类大数据资产并整理清理候选；支持查看证据、追问、停止和导出 Markdown。分析按需补查文件元数据，不执行清理。详见 [Agent 分析设计与接口](docs/agent.md)。

## 配置与数据

- 数据目录默认 `data/`，通过 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR` 指定。
- Docker 归属标签默认 `project-alpha.owner`，可在扫描配置中修改，也可在网页设置容器归属。
- 扫描方式：`host` 使用服务自身权限，`docker` 使用只读辅助容器；默认 `auto` 在启用 Docker 的普通用户下优先使用辅助容器，否则直接扫描。自动模式仅在辅助容器尚未开始扫描时允许回退。
- 辅助容器默认使用本机已有的 `ubuntu:latest`，通过 `PROJECT_ALPHA_SCAN_HELPER_IMAGE` 指定含 `/usr/sbin/chroot` 的镜像，不自动拉取。服务与 Docker daemon 必须共享宿主机路径视图。
- 扫描模式：默认 `normal` 最多使用 4 路 Go 并发，`fast` 使用进程可见的全部逻辑 CPU。目录遍历为串行，速度仍受磁盘 I/O 限制。

管理员可在“存储 → 诊断清理”让 Agent 读取完整报告并提取全部路径和说明，筛选、勾选并确认后后台清空实际目录或删除文件。清理会保留所选目录本身和权限（包括 `/tmp` 的 sticky bit），跳过 socket、字符设备并保留其所在目录，结果显示跳过数量。容器可写层通过 `docker exec` 清理，要求容器内有 Python 3（含 ctypes）；容器停止、暂停、只读或缺少依赖时在删除前报错。已结束的提取记录可单独删除并从原报告重新提取，不影响实际磁盘内容。每次删除需在确认窗口输入服务账号的 sudo 密码，仅通过内存管道用于本批提权，不保存密码或复用 sudo 授权；处理结果持久保存，清理后需重新扫描更新空间统计。

SQLite 保存账号、配置和任务；扫描结果保存在 `data/results/`。目录增量更新按节点写入 SQLite，取消时保留已提交的明细。历史记录可在网页删除；备份时停止服务并复制整个数据目录。

1.0 发布前不保证任何前向或后向兼容性，包括数据库表结构、配置、API 和快照格式；不维护旧格式迁移或兼容分支。当前数据库格式为 v11、快照为 v3；格式不匹配时使用新的数据目录，重新配置并扫描。程序不会自动删除已有数据。

独立扫描示例：

```bash
./bin/project-alpha scan --no-docker --root /srv/models --output snapshots/latest.json
./bin/project-alpha scan --help
```

CLI 与 API 使用同一套配置校验：`max_depth` 为 0–32，`max_nodes` 为 100–100000，`docker_timeout` 为 5–3600 秒；扫描及排除目录必须为绝对路径。

## 容器进程监控

`process` 子命令订阅 Tetragon 的进程事件，维护每个容器的活动进程森林并导出 JSON，供前端展示容器内正在运行的进程及其命令行。

```bash
./bin/project-alpha process --duration 30s --output process-forest.json
```

需要本机运行 Tetragon agent，并具备访问 `/var/run/tetragon/tetragon.sock` 的权限，默认路径可用 `--socket` 修改。采集开始时先读取 Tetragon 进程缓存，因此采集前就已运行的进程也会出现在森林中；之后按 exec/exit 事件增删节点，进程退出后其子进程上移到最近的存活祖先。输出按容器分组，容器标识使用 Tetragon 记录的容器 ID（Docker 下为 15 位前缀）；`--host` 可一并导出没有容器标识的主机进程。

`serve` 也常驻订阅同一份事件，状态全部保存在内存中，不写入数据库。每轮采集先建立事件订阅，再读取进程缓存建树，最后回放订阅到建树之间到达的事件，因此冷启动窗口不会丢事件；事件流断开会自动退避重连并重新建树。

进程管理以容器名称为标题，下面显示 Tetragon 容器 ID，支持按进程数或名称字母序升降序排列，并可搜索名称或 ID。名称来自 Tetragon 元数据及 Docker 容器列表，Docker 名称每 30 秒刷新一次，无需先扫描磁盘；无法获取名称时标为“未识别名称”，仍保留 ID 和进程数据。

Tetragon 自身的缓冲区可能丢事件且客户端无法感知，因此每 30 秒用进程缓存校准一次：缓存里有、内存里没有的进程补回树中；缓存里没有的进程视为漏收了 exit，连续两次校准都缺失才删除——刚启动、还没进缓存的进程不受影响。

Tetragon 的进程缓存有容量上限，被打满时会淘汰条目，**包括仍在运行的进程**；被淘汰的进程不会再出现在任何一次缓存导出里（Tetragon 自己也失去了它的父子关系），所以只靠缓存无法把它补回来。因此删除前会先探一次 `/proc/<pid>`，比对启动时间以排除 PID 复用：只要它还在运行就不删，并且每轮校准重新探，这样它真正退出时仍会被正常清掉。原地 exec 不会为旧映像产生 exit 事件，但新映像带着同一个 PID、容器和启动时间出现，因此会把旧节点就地改写为新映像，而不是并排留下两份：节点数不变、子进程仍挂在同一任务下，探针也就能继续保护存活的那个映像（此前旧映像会让 PID 变得无法分辨，导致被淘汰的存活新映像被误删）。只有确实分辨不出时才不启用探针，即同一个 PID 上出现启动时间不同的两个节点（PID 被内核复用给另一个进程），此时仍由缺失计数清理。探针只能否决删除、不会导致删除——`/proc` 不可读时行为与没有探针完全一致，最坏情况是树里多留一个无害的条目，绝不会误删。被淘汰但仍存活的进程计入 `status.alignment.evicted`，这个数持续增长说明 agent 的进程缓存偏小。

已登录用户可读取当前森林，`?host=1` 一并包含主机进程：

```bash
curl -b cookie.txt 'http://127.0.0.1:8765/api/process/forest?host=1'
```

返回 `{"status":{...},"forest":{...}}`。`status` 说明是否已连接、是否成功建树、重连次数和最近错误，其中 `alignment` 是对齐估计：最近对齐时间、`matched`/`added`/`ghosts`/`evicted`/`removed` 计数，以及 `aligned_percent`（内存与缓存一致的进程占比，补回的缺失、待删的幽灵和被淘汰但存活的进程都会拉低它）。这是最大努力的估计，不保证与真实进程表完全一致。未连接 Tetragon 时服务照常启动，该接口返回 503；socket 路径可用 `serve --tetragon-socket` 指定。

没有 Tetragon 时，可用 `tetra getevents -o json` 导出的事件离线回放：

```bash
./bin/project-alpha process --events-file events.jsonl --output process-forest.json
```

`tests/fixtures/tetragon-events.jsonl` 是随仓库提供的回放样例，通过 `go run ./examples/process-events` 重新生成。

## 部署

将二进制放到 `/opt/project-alpha/bin/project-alpha`，安装 [systemd 单元](deploy/project-alpha.service)：

```bash
sudo cp deploy/project-alpha.service /etc/systemd/system/project-alpha.service
sudo systemctl daemon-reload
sudo systemctl enable --now project-alpha
```

服务默认监听回环地址。远程访问可使用 SSH 转发：

```bash
ssh -L 8765:127.0.0.1:8765 user@your-server
```

HTTPS 部署参考 [nginx 配置](deploy/nginx.conf.example)，在服务参数中添加 `--allowed-host <域名> --secure-cookie`。

## 验证

前端测试要求 Node.js 20+，无 npm 依赖。

```bash
go test -race ./...
go vet ./...
for test in tests/test_*.js; do node "$test" || exit; done
```

浏览器回归：安装 Playwright 和 Chromium 后运行 `python3 tests/test_live_map_browser.py`、`python3 tests/test_agent_browser.py`、`python3 tests/test_cleanup_browser.py`、`python3 tests/test_workspace_browser.py` 和 `python3 tests/test_auth_browser.py`，验证目录下钻、报告生成、追问、导出，以及登录开屏的逐笔绘制、跳过/重播、减少动态效果、总面板、模块导航、进程监控、权限和移动端布局。Agent 回归使用本机模拟模型接口，不产生真实模型调用。具备 Docker 权限和辅助镜像时，可运行 `PROJECT_ALPHA_TEST_DOCKER_MODES=1 go test -run '^TestDockerHelper(ScanModes|SlowResult)Integration$' -v ./internal/storage`，验证扫描模式及慢速接收时的结果完整性。

Docker 清理回归：`PROJECT_ALPHA_TEST_OVERLAY_CLEANUP=1 python3 tests/test_cleanup_overlay.py` 使用本地 `python:3.12-slim`（可用 `PROJECT_ALPHA_CLEANUP_TEST_IMAGE` 指定含 Python 3 的镜像）创建并自动移除专用测试容器和镜像，不拉取镜像、不操作已有业务容器。验证真实 `docker exec` 清理、whiteout、空间释放、重复清理、socket 与字符设备保留，以及停止、只读、嵌套挂载、符号链接保护和断线取消。

测试数据位于 `tests/fixtures/snapshot.json`，通过 `go run ./examples --output tests/fixtures/snapshot.json` 重新生成。

## 参考

- [存储统计口径](docs/accounting.md)
- [Agent API](docs/agent.md)
