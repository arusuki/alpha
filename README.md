# project alpha

集群管理平台：总控统一提供 Web 界面、账号和使用者管理，每个 node 以 API-only worker 运行并维护独立 SQLite 数据库。登录后从集群总控选择 node，进入该主机的存储、容器管理和进程管理；Agent 配置集中在总控首页。自动发现 Docker 容器，按容器和用户汇总存储占用，支持目录下钻、按需扫描、任务管理和 Agent 空间报告。后端使用 Go 和 SQLite，网页资源嵌入二进制。

## 代码结构

- `cmd/project-alpha`：程序入口，处理进程信号并启动应用。
- `cmd/rootless-docker`、`internal/rootless`：独立 rootless Docker 管理命令、socket 热挂载和交互测试容器；不接入 Web。
- `internal/app`：命令分发、模块装配和 HTTP 服务生命周期。
- `internal/cluster`：节点注册与身份核验、总控代理、权限授权、跨节点容器统计和 API-only worker 入口。
- `internal/platform`：平台 HTTP 入口、账号与会话、访问校验、审计和数据库基础；不依赖存储模块。
- `internal/storage`：扫描配置、Docker 发现与辅助扫描、任务调度、快照与增量更新、共享记录读取与探索服务，以及对应 API 和数据表。
- `internal/agent`：模型配置与凭据、Responses/Chat Completions 适配、分析会话、工具定义与编排，以及独立 API 和数据表。
- `internal/bastion`、`internal/tailscale`：共享访问资源池、Tailscale 凭据及邀请、本机跳板公钥和回收记录。
- `internal/members`：独立的机器使用者、注册 schema、邀请码页面管理和公开注册 API。
- `internal/containers`：命令行扫描导入、创建配置、启停与删除，以及管理记录和审计。
- `internal/process`：订阅 Tetragon 进程事件，常驻维护并按容器导出活动进程森林。
- `internal/httpapi`、`internal/fsutil`：共用的 HTTP/JSON 处理与路径规范化。
- `dist`：网页资源及 Go 嵌入声明；`tests`：前端回归和共享测试数据。Go 测试与所属包放在一起。

应用层按 `--control` / `--worker` 装配模块：总控运行集群、使用者管理和 Agent，node 运行存储、容器和进程；平台完成会话与请求校验后，通过 `platform.Module` 分发业务请求。各模块的数据表由各自维护的 `schema.sql` 定义，在同一事务中初始化。Agent 通过 `agent.Records` 的远程实现调用所选 node 的 `storage.Service`；模型配置和对话均保存在总控，节点只执行工具能力。Agent 生命周期和完成状态重试独立于扫描管理器。

Web 的快照、变更读取和目录探索，以及 Agent 的查询工具，共用 `storage.Service`。探索使用原记录 ID、`revision` 和 1–32 层 `depth`，发布到同一条记录；两边都能读到最新已提交版本。目录文件统计也随记录持久化，不维护 Agent 私有的目录快照或缓存。详见 [记录读取与探索](docs/records.md)。

## 启动

要求 Linux、Go 1.26+ 和 GCC；Docker 扫描需要本机 Docker CLI 及 daemon 访问权限。

```bash
go build -o bin/project-alpha ./cmd/project-alpha
./bin/project-alpha --control --data-dir ./control-data
```

在每台 node 上启动 worker，未指定令牌时会自动生成强随机令牌并在命令行打印：

```bash
./bin/project-alpha --worker --data-dir ./node-data --host 0.0.0.0 --port 8766
```

打开总控 <http://127.0.0.1:8765> 创建管理员，在“集群总控 → 添加 node”填写节点名称、总控可访问的 API 地址及令牌。然后进入节点的“存储 → 扫描配置”开始扫描。node 不提供网页、登录或注册入口。未指定模式时启动总控；`--control` 和 `--worker` 互斥。

自动生成的令牌包含 256 位随机熵（64 位十六进制字符），以 0600 权限保存在节点数据目录的 `worker-token` 文件中，重启时复用，原地址恢复后无需重新登记。需要自行指定令牌时，可通过 `--worker-token-file ./node-token`（建议文件权限 0600）或 `PROJECT_ALPHA_WORKER_TOKEN` 提供；指定文件时以文件为准，显式提供的令牌不打印。

浏览器只连接总控，全部节点请求由总控认证并代理。只读账号可查看所有节点；“集群使用者”统一登记机器使用者，“使用者容器”按使用者、节点和容器统计。详细部署、权限和 API 说明见 [集群管理](docs/cluster.md)。

管理员在总控配置“Agent 设置 → API Key 与模型”后，可在空间用量页分别生成 Host 和容器空间报告。Host 报告分析显式配置的宿主机扫描根中未关联容器的物理空间；容器报告排查挂载、可写层和其他容器资源。两类报告分别显示分析记录、分组 Agent 与清理提取入口，支持查看证据、追问、停止和导出 Markdown。分析按需补查文件元数据，不执行清理。详见 [Agent 分析设计与接口](docs/agent.md)。

## 配置与数据

- 总控和每个 node 使用不同数据目录；目录绑定运行角色，不可切换。数据目录默认 `data/`，通过 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR` 指定。
- Docker 归属标签默认 `project-alpha.owner`，可在扫描配置中修改，也可在网页设置容器归属。
- 扫描方式：`host` 使用服务自身权限，`docker` 使用只读辅助容器；默认 `auto` 在启用 Docker 的普通用户下使用辅助容器，否则直接扫描。辅助容器不可用时任务直接失败，不再回退到权限不足的宿主机扫描。
- 辅助容器默认使用 `alpine:latest`（其中的 `/usr/sbin/chroot` 用于进入宿主机根），可用 `PROJECT_ALPHA_SCAN_HELPER_IMAGE` 指定其它镜像。本机不存在该镜像时会自动 `docker pull` 一次；拉取失败或辅助容器无法启动时任务报错，不会静默降级。服务与 Docker daemon 必须共享宿主机路径视图。
- 扫描模式：默认 `normal` 最多使用 4 路 Go 并发，`fast` 使用进程可见的全部逻辑 CPU。目录遍历为串行，速度仍受磁盘 I/O 限制。

管理员可在“存储 → 诊断清理”分别选择 Host 或容器完整报告，让 Agent 提取各自报告的全部路径和说明；提取结果与状态按来源隔离，共用筛选、勾选、确认和后台删除界面。Host 清理只接受经扫描记录核实、整棵子树均未关联容器的物理路径，混合目录须先下钻。清理会保留所选目录本身和权限（包括 `/tmp` 的 sticky bit），跳过 socket、字符设备并保留其所在目录，结果显示跳过数量。容器可写层通过 `docker exec` 清理，要求容器内有 Python 3（含 ctypes）；容器停止、暂停、只读或缺少依赖时在删除前报错。已结束的提取记录可单独删除并从原报告重新提取，不影响实际磁盘内容。每次删除需在确认窗口输入服务账号的 sudo 密码，仅通过内存管道用于本批提权，不保存密码或复用 sudo 授权；处理结果持久保存，清理后需重新扫描更新空间统计。

总控 SQLite 保存账号、使用者、节点连接（含节点令牌）以及 Agent 配置、会话和报告，node SQLite 保存各自扫描配置、任务、容器和审计；Agent API Key 加密后存入总控 SQLite，密钥保存在总控数据目录的 `agent-api-key.key`。扫描结果保存在 `data/results/`。目录增量更新按节点写入 SQLite，取消时保留已提交的明细。历史记录可在网页删除；备份时停止服务并复制整个数据目录，包括密钥文件。

1.0 发布前不保证任何前向或后向兼容性，包括数据库表结构、配置、API 和快照格式；不维护旧格式迁移或兼容分支。当前数据库格式为 v19、快照为 v5；格式不匹配时使用新的数据目录，重新配置并扫描。程序不会自动删除已有数据。

独立扫描示例：

```bash
./bin/project-alpha scan --no-docker --root /srv/models --output snapshots/latest.json
./bin/project-alpha scan --help
```

CLI 与 API 使用同一套配置校验：`max_depth` 为 0–32，`max_nodes` 为 100–100000，`docker_timeout` 为 5–3600 秒；扫描及排除目录必须为绝对路径。

## 容器创建与接管

“容器管理”直接调用 Docker，无需生成或执行 Compose 文件。管理员可在网页创建、启停、重启、删除和解除接管；已有容器使用命令行扫描导入；只读账号可查看管理记录和连接信息，无需先进行存储扫描。

先在页面配置本机 Docker Unix socket、默认镜像、数据根目录（默认 `/docker`）、SSH 起始端口、主机地址及可选 ProxyJump。

创建容器时使用 `docker-<名称>` 主机名、host IPC、TTY、`unless-stopped`、无限 memlock、NVIDIA GPU（all 或 1–7）、bridge/host 网络，以及以下可写 bind 挂载：

- `<数据根>/<名称>/workspace` → `/workspace`
- `<数据根>/<名称>/home` → `/home`
- `<数据根>/data` → `/data`

镜像必须已经在本机准备好，并包含 Bash、sed、OpenSSH server 和 `chpasswd`。平台不自动拉取镜像。SSH 端口可指定或从起始值及已有容器端口之后分配，并检查宿主机监听冲突。bridge 的可选代理仍为 `http://localhost:7890`，该地址指向容器自身。新密码经标准输入传给 `chpasswd`，不写入启动命令、数据库或审计；SSH 初始化完成后才启用，密码仅在创建/初始化结果中显示一次。初始化失败会保留可重试的管理记录。已存在的个人数据目录不自动复用或覆盖。

已有容器通过命令行直接导入平台数据目录，无需启动 Web 服务或登录：

```bash
# 可选：只检查，不登记
./project-alpha containers import --data-dir ./node-data --dry-run
# 扫描全部现有容器，登记符合条件的容器
./project-alpha containers import --data-dir ./node-data
# 指定挂载根目录和待导入容器（省略容器名时扫描全部）
./project-alpha containers import --data-dir ./node-data --base-dir /docker alice bob
```

`--data-dir` 默认取 `PROJECT_ALPHA_DATA_DIR`，未设置时为 `data`。`--endpoint` 和 `--base-dir` 默认使用此数据目录的容器配置，新目录分别为 `unix:///var/run/docker.sock` 和 `/docker`；成功登记时保存显式指定的配置。`--data-dir` 是平台 SQLite 数据目录，`--base-dir` 是已有容器的 bind 挂载根目录。首次运行会初始化平台数据库，包括 `--dry-run`；预检不写入容器记录、归属、配置或审计。旧格式数据目录会明确报错，不迁移或覆盖。

导入默认以容器名作为所属用户，同步到存储模块的归属覆盖记录。重复执行会跳过已登记的容器；失败项逐项显示原因，不影响其他符合条件的容器，存在失败项时退出码非零。导入范围仍是原脚本约定的训练容器，检查完整 ID、daemon、运行状态、root、主机名、TTY/IPC/重启策略、memlock、GPU、三个可写 bind 挂载和宿主机目录，以及网络和 `sshd -T` 的有效端口。停止或配置不符的容器需修复后重试，不强制跳过检查。

导入不重建、重启或更改原密码，原 Compose 标签和命令保留；**导入成功后停止用原 Compose 文件操作该容器**。在对应 node 上执行导入，必须指定它的 worker 数据目录；建议先停止该 node，完成导入后使用同一个 `--data-dir` 和 `--worker` 启动；刷新容器管理页面即可查看记录。`sshd -T` 验证有效配置，不代表外部网络或防火墙一定可达。

所有启停/删除操作使用登记的完整 ID，并重新核实 daemon 和配置；同名替换容器不会成为操作目标。外部修改后需解除接管并通过命令行重新导入。解除接管只移除管理记录，不操作 Docker，也可用于清理已不存在的容器记录。删除要求输入完整容器名且先停止容器，不强制删除，不删除挂载目录或数据卷；容器可写层（包括未持久化的 `/root`）会丢失。操作超时后先刷新确认实际状态再重试。

API、检查范围和失败恢复详见 [容器管理](docs/containers.md)。旧数据目录按项目约定不迁移，请使用新数据目录。已有 Docker 容器独立于平台数据库，可通过命令行重新导入新目录。

## 集群使用者

管理员通过「集群使用者」配置注册信息 schema（文本、单选、必填）、在页面生成带 quota 的邀请码，并查看使用者信息；邀请码管理不提供 JSON API。使用者通过 `GET /api/members/registration-schema` 获取表单定义，再调用 `POST /api/members/register` 提交使用者标识、邀请码、SSH 公钥和信息。每个邀请码只允许成功登记 quota 人，名额用尽后失效，失败不扣名额。

这些账号属于机器使用者，独立于 Alpha 运维平台登录账号；注册不提供平台访问权限。总控在注册后分配一个 Tailscale 分享节点和一个本机跳板账号，将 SSH 公钥注入跳板及各 node 的容器。每名使用者在每个 node 最多分配一个容器；凭本人资源令牌查询和补申请失败项。管理员删除使用者时逐项回收资源，离线或失败记录保留到回收完成。详见 [跳板机管理与资源 API](docs/bastion.md)。接口、示例和校验规则见 [集群使用者登记](docs/members.md)。

使用者打开总控 `/status/<注册返回的id>`，输入本人资源令牌，即可查看全部 node 的基本信息、在线状态和自己的容器名称。尚无容器的在线节点可点击「＋」立即申请，成功显示容器和 SSH 地址，失败显示具体错误并可重试；管理员使用者列表提供状态页链接。

## 独立 rootless Docker 工具

原 `rootless` 目录的管理工具与交互测试脚本已用 Go 实现为独立命令，支持专用用户/服务初始化、代理与宿主机回环配置、服务管理、Docker CLI 透传，以及向已有运行容器热挂载 socket。

```bash
go build -o bin/rootless-docker ./cmd/rootless-docker
sudo ./bin/rootless-docker init
sudo ./bin/rootless-docker add <容器名或ID>
sudo ./bin/rootless-docker docker ps -a
sudo ./bin/rootless-docker test exec
```

需要 Linux 宿主机、systemd、Docker Engine 和 rootless 依赖。配置及数据保存在专用用户家目录，不使用平台数据库。容器或 rootless daemon 重启后需重新执行 `add`；`test` 子命令会自动重新挂载。完整命令、代理配置、权限边界与验证方式见 [rootless Docker 工具](docs/rootless-docker.md)。

## 容器进程监控

`process` 子命令订阅 Tetragon 的进程事件，维护每个容器的活动进程森林并导出 JSON，供前端展示容器内正在运行的进程及其命令行。

```bash
./bin/project-alpha process --duration 30s --output process-forest.json
```

需要本机运行 Tetragon agent，并具备访问 `/var/run/tetragon/tetragon.sock` 的权限，默认路径可用 `--socket` 修改。采集开始时先读取 Tetragon 进程缓存，因此采集前就已运行的进程也会出现在森林中；之后按 exec/exit 事件增删节点，进程退出后其子进程上移到最近的存活祖先。输出按容器分组，容器标识使用 Tetragon 记录的容器 ID（Docker 下为 15 位前缀）；`--host` 可一并导出没有容器标识的主机进程。

`serve --worker` 常驻订阅同一份事件，状态全部保存在内存中，不写入数据库。每轮采集先建立事件订阅，再读取进程缓存建树，最后回放订阅到建树之间到达的事件，因此冷启动窗口不会丢事件；事件流断开会自动退避重连并重新建树。

进程管理以容器名称为标题，下面显示 Tetragon 容器 ID，支持按进程数或名称字母序升降序排列，并可搜索名称或 ID。名称来自 Tetragon 元数据及 Docker 容器列表，Docker 名称每 30 秒刷新一次，无需先扫描磁盘；无法获取名称时标为“未识别名称”，仍保留 ID 和进程数据。

Tetragon 自身的缓冲区可能丢事件且客户端无法感知，因此每 30 秒用进程缓存校准一次：缓存里有、内存里没有的进程补回树中；缓存里没有的进程视为漏收了 exit，连续两次校准都缺失才删除——刚启动、还没进缓存的进程不受影响。

Tetragon 的进程缓存有容量上限，被打满时会淘汰条目，**包括仍在运行的进程**；被淘汰的进程不会再出现在任何一次缓存导出里（Tetragon 自己也失去了它的父子关系），所以只靠缓存无法把它补回来。因此删除前会先探一次 `/proc/<pid>`，比对启动时间以排除 PID 复用：只要它还在运行就不删，并且每轮校准重新探，这样它真正退出时仍会被正常清掉。原地 exec 不会为旧映像产生 exit 事件，但新映像带着同一个 PID、容器和启动时间出现，因此会把旧节点就地改写为新映像，而不是并排留下两份：节点数不变、子进程仍挂在同一任务下，探针也就能继续保护存活的那个映像（此前旧映像会让 PID 变得无法分辨，导致被淘汰的存活新映像被误删）。只有确实分辨不出时才不启用探针，即同一个 PID 上出现启动时间不同的两个节点（PID 被内核复用给另一个进程），此时仍由缺失计数清理。探针只能否决删除、不会导致删除——`/proc` 不可读时行为与没有探针完全一致，最坏情况是树里多留一个无害的条目，绝不会误删。被淘汰但仍存活的进程计入 `status.alignment.evicted`，这个数持续增长说明 agent 的进程缓存偏小。

已登录用户可读取当前森林，`?host=1` 一并包含主机进程：

```bash
curl -b cookie.txt 'http://127.0.0.1:8765/api/cluster/nodes/<节点ID>/api/process/forest?host=1'
```

返回 `{"status":{...},"forest":{...}}`。`status` 说明是否已连接、是否成功建树、重连次数和最近错误，其中 `alignment` 是对齐估计：最近对齐时间、`matched`/`added`/`ghosts`/`evicted`/`removed` 计数，以及 `aligned_percent`（内存与缓存一致的进程占比，补回的缺失、待删的幽灵和被淘汰但存活的进程都会拉低它）。这是最大努力的估计，不保证与真实进程表完全一致。未连接 Tetragon 时服务照常启动，该接口返回 503；socket 路径可用 `serve --worker --tetragon-socket` 指定。

没有 Tetragon 时，可用 `tetra getevents -o json` 导出的事件离线回放：

```bash
./bin/project-alpha process --events-file events.jsonl --output process-forest.json
```

`tests/fixtures/tetragon-events.jsonl` 是随仓库提供的回放样例，通过 `go run ./examples/process-events` 重新生成。

## 部署

将二进制放到 `/opt/project-alpha/bin/project-alpha`，在总控安装 [systemd 单元](deploy/project-alpha.service)：

```bash
sudo cp deploy/project-alpha.service /etc/systemd/system/project-alpha.service
sudo systemctl daemon-reload
sudo systemctl enable --now project-alpha
```

在每台 node 安装 [worker systemd 单元](deploy/project-alpha-worker.service)，并先准备 `/etc/project-alpha/node-token`（权限 0600）。总控与 worker 的默认示例数据目录分别为 `/var/lib/project-alpha-control` 和 `/var/lib/project-alpha-worker`。

总控服务默认监听回环地址。远程访问可使用 SSH 转发：

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

使用者注册端到端回归：`python3 tests/test_members_browser.py` 会构建并启动使用临时数据目录的本机服务，验证管理员字段配置、邀请码页面管理、公开注册、配额、权限隔离和移动端布局；不操作 Docker。

使用者状态页回归：`python3 tests/test_status_browser.py` 使用本机模拟 API，验证令牌、已有/未分配/离线节点、创建成功与失败、重复点击、退出时的在途请求和移动布局；后端身份隔离与同步申请由 `go test -race ./internal/cluster` 验证。

浏览器回归：`python3 tests/test_cluster_browser.py` 启动真实总控及两个 worker，验证 API-only 节点、集中注册、跨节点容器统计、扫描隔离、快照代理、离线状态与只读权限。安装 Playwright 和 Chromium 后也可运行 `python3 tests/test_live_map_browser.py`、`python3 tests/test_agent_browser.py`、`python3 tests/test_cleanup_browser.py`、`python3 tests/test_workspace_browser.py` 和 `python3 tests/test_auth_browser.py`，验证目录下钻、报告生成、追问、导出，以及登录开屏的逐笔绘制、跳过/重播、减少动态效果、总面板、模块导航、进程监控、权限和移动端布局。Agent 回归使用本机模拟模型接口，不产生真实模型调用。具备 Docker 权限和辅助镜像时，可运行 `PROJECT_ALPHA_TEST_DOCKER_MODES=1 go test -run '^TestDockerHelper(ScanModes|SlowResult)Integration$' -v ./internal/storage`，验证扫描模式及慢速接收时的结果完整性。

Docker 清理回归：`PROJECT_ALPHA_TEST_OVERLAY_CLEANUP=1 python3 tests/test_cleanup_overlay.py` 使用本地 `python:3.12-slim`（可用 `PROJECT_ALPHA_CLEANUP_TEST_IMAGE` 指定含 Python 3 的镜像）创建并自动移除专用测试容器和镜像，不拉取镜像、不操作已有业务容器。验证真实 `docker exec` 清理、whiteout、空间释放、重复清理、socket 与字符设备保留，以及停止、只读、嵌套挂载、符号链接保护和断线取消。

存储性能基准：`go test ./internal/storage -run '^$' -bench '^(BenchmarkIncrementalScan|BenchmarkDirectoryMerge|BenchmarkSnapshotPrepare)$' -benchmem`。比较结果时使用相同的迭代数和 CPU 配置。

测试数据位于 `tests/fixtures/snapshot.json`，通过 `go run ./examples --output tests/fixtures/snapshot.json` 重新生成。

## 参考

- [存储统计口径](docs/accounting.md)
- [Agent API](docs/agent.md)
