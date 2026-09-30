# project alpha

当前版本：`0.1.0`。

集群管理平台。总控提供网页、账号、使用者管理和 Agent 分析；每台节点运行 worker，负责存储扫描、容器管理和进程监控。后端使用 Go 和 SQLite，网页资源嵌入二进制。

## 启动

需要 Linux、Go 1.26+ 和 GCC。Docker 功能需要本机 Docker CLI 及 daemon 访问权限。

```bash
go build -o bin/project-alpha ./cmd/project-alpha
./bin/project-alpha --control --data-dir ./control-data
```

打开 <http://127.0.0.1:8765> 创建管理员。在每台节点启动 worker：

```bash
./bin/project-alpha --worker --data-dir ./node-data --host 0.0.0.0 --port 8766
```

worker 启动时生成并打印连接令牌，以 0600 权限保存到数据目录的 `worker-token`，重启后复用。可用 `--worker-token-file` 或 `PROJECT_ALPHA_WORKER_TOKEN` 显式提供令牌；文件优先，显式令牌不打印。

在总控“添加节点”填写 worker 名称、API 根地址和令牌，进入节点后配置扫描。浏览器请求由总控认证并代理。总控首页管理 Agent、集群使用者和账号；节点页面提供存储、容器和进程管理。部署及接口见 [集群管理](docs/cluster.md)。

## Linux Release 与 CI

[Linux CI and Release](.github/workflows/release.yml) 在推送到 `master`、向 `master` 提交 PR 或手动运行时，执行 Go race 测试、`go vet` 和前端 JavaScript 测试，再构建 Linux amd64 / arm64 安装包。普通构建的安装包可在 Actions 页面的 Artifacts 下载，保留 14 天；不会创建 Release。

发布时，在包含此工作流的提交上创建并推送版本标签，例如：

```bash
git tag v0.1.0
git push origin v0.1.0
```

`v*` 标签通过全部检查和双架构构建后，自动创建 GitHub Release 并上传：

- `project-alpha_<标签>_linux_amd64.tar.gz`
- `project-alpha_<标签>_linux_arm64.tar.gz`
- `SHA256SUMS`

带连字符的标签（例如 `v0.2.0-rc.1`）标记为预发布。失败后可在 Actions 重跑；已有 Release 的同名附件会被替换。手动运行只生成 Artifacts，不发布 Release。使用仓库内置的 `GITHUB_TOKEN`，无需额外配置 Secret。

压缩包包含 `bin/project-alpha`、`bin/rootless-docker`、`README.md`、`docs/`、`deploy/` 和记录版本、提交、架构及 Go 版本的 `BUILD_INFO`。网页资源已嵌入主程序，无需另行构建前端。

二进制在 Ubuntu 24.04 上原生编译，启用 CGO 以支持 SQLite；运行环境使用 glibc 2.39 或更新版本（例如 Ubuntu 24.04），不直接支持 Alpine/musl。使用发布包无需安装 Go 或 GCC；Docker 等功能仍需对应的运行时依赖。较旧的 Linux 发行版可按“启动”章节从源码构建。

下载对应架构的压缩包和校验文件后，例如：

```bash
# 仅校验已下载的架构；同时下载两种架构时也可去掉 --ignore-missing。
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf project-alpha_v0.1.0_linux_amd64.tar.gz
cd project-alpha_v0.1.0_linux_amd64
./bin/project-alpha --control --data-dir ./control-data
```

## 公网 registry

公网注册使用 registry，总控主动建立出站连接：

```bash
REG_PASS=Ab3dE6gH ./bin/project-alpha --registry --data-dir ./registry-data --port 8767 \
  --allowed-host register.example.com --secure-cookie
```

在总控添加 registry 连接，并配置邀请码和使用者资源。注册入口为 `https://register.example.com/registry/Ab3dE6gH/<邀请码>`。部署参考 [registry systemd 单元](deploy/project-alpha-registry.service) 和 [nginx 示例](deploy/nginx-registry.conf.example)，登记规则见 [使用者登记](docs/members.md)。

## 配置与数据

总控、worker 和 registry 各使用独立数据目录，目录绑定运行角色。默认目录为 `data/`，可通过 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR` 指定。未指定服务模式时启动总控；`--control`、`--worker` 和 `--registry` 互斥。

数据库格式为 v24，快照格式为 v5。1.0 发布前不保证格式兼容，不提供旧格式迁移；格式不匹配时使用新数据目录，程序保留已有数据。

总控保存账号、使用者、节点连接、Agent 配置、会话、报告和清理记录；worker 保存扫描配置、任务、容器和审计。扫描结果位于 `results/`。Agent API Key 加密存入 SQLite，密钥位于总控数据目录的 `agent-api-key.key`。备份时停止服务，复制整个数据目录及密钥文件。

扫描后端 `host` 使用服务账号权限，`docker` 使用只读辅助容器；默认 `auto` 在启用 Docker 的普通账号下使用辅助容器，否则直接扫描。辅助容器使用 `alpine:latest`，可通过 `PROJECT_ALPHA_SCAN_HELPER_IMAGE` 指定；本地缺少时拉取，启动失败时任务报错。扫描模式 `normal` 最多使用 4 路并发，`fast` 使用进程可见的全部逻辑 CPU。

独立扫描：

```bash
./bin/project-alpha scan --no-docker --root /srv/models --output snapshots/latest.json
./bin/project-alpha scan --help
```

统计定义见 [存储统计口径](docs/accounting.md)，快照和目录探索见 [记录读取与探索](docs/records.md)。

管理员可在空间用量页生成 Host 或容器报告，在“诊断清理”读取报告条目并选择清理范围。每批删除需输入所选 worker 系统服务账号的 sudo 密码；处理结果持久保存，清理后重新扫描更新用量。报告、权限与接口见 [Agent API](docs/agent.md)。

## 容器与使用者

容器管理支持创建、启停、重启、删除和解除接管。创建前配置本机 Docker socket、镜像、数据根目录、SSH 端口和连接地址；owner 使用已登记的集群使用者标识。已有容器在对应 worker 上导入：

```bash
./bin/project-alpha containers import --data-dir ./node-data --dry-run
./bin/project-alpha containers import --data-dir ./node-data
```

导入检查、数据挂载和失败恢复见 [容器管理](docs/containers.md)。使用者注册可分配 Tailscale 分享、`alpha-jump` 公钥及 worker 容器，资源配置见 [跳板机与使用者资源](docs/bastion.md)。

独立 rootless Docker 工具支持初始化、服务管理、Docker CLI 透传及向运行容器热挂载 socket：

```bash
go build -o bin/rootless-docker ./cmd/rootless-docker
sudo ./bin/rootless-docker init
sudo ./bin/rootless-docker add <容器名或ID>
```

命令和权限说明见 [rootless Docker 工具](docs/rootless-docker.md)。

## 进程监控

worker 通过 Tetragon 事件和进程缓存维护活动进程树，默认 socket 为 `/var/run/tetragon/tetragon.sock`，可用 `--tetragon-socket` 配置。节点接口 `GET /api/process/forest?host=1` 返回连接状态和进程树；未连接时返回 503。

每 30 秒校准缓存，连续两次缺失且无法从宿主机 `/proc` 确认仍在运行的节点会从显示中移除。缓存淘汰、丢失事件、不可读的 procfs 或 PID 命名空间不一致都可能导致显示遗漏。校准只维护显示数据。

```bash
./bin/project-alpha process --duration 30s --output process-forest.json
./bin/project-alpha process --events-file tests/fixtures/tetragon-events.jsonl --output process-forest.json
```

## 部署

将二进制放到 `/opt/project-alpha/bin/project-alpha`，调整 [总控 systemd 单元](deploy/project-alpha.service) 的 `User=` 为实际普通服务账号，再安装并启动。每台节点使用 [worker 单元](deploy/project-alpha-worker.service)，准备权限为 0600 的 `/etc/project-alpha/node-token`。HTTPS 部署参考 [nginx 示例](deploy/nginx.conf.example)，配置 `--allowed-host` 和 `--secure-cookie`。

总控默认监听回环地址，也可使用 SSH 转发访问：

```bash
ssh -L 8765:127.0.0.1:8765 user@your-server
```

## 验证

前端测试要求 Node.js 20+，无 npm 依赖。

```bash
go test -race ./...
go vet ./...
for test in tests/test_*.js; do node "$test" || exit; done
```

浏览器回归需要 Playwright 和 Chromium：

```bash
python3 tests/test_members_browser.py
python3 tests/test_registry_browser.py
python3 tests/test_cluster_browser.py
python3 tests/test_status_browser.py
```

测试使用临时数据及模拟模型、Tailscale 或资源响应。其余浏览器测试在 `tests/test_*_browser.py`，覆盖工作台、登录、Agent、目录探索和清理。

`python3 tests/test_bastion_ssh.py` 在无网络临时容器内验证真实 SSH、sudo 及跨 control / 服务用户接管。默认使用已有 `sc2025:1.0` 镜像，可用 `PROJECT_ALPHA_BASTION_IMAGE` 指定含 OpenSSH、sudo、Python 3 和用户管理工具的镜像；不拉取镜像，不修改宿主账号或 SSH。

具备 Docker 权限和辅助镜像时，可运行 `PROJECT_ALPHA_TEST_DOCKER_MODES=1 go test -run '^TestDockerHelper(ScanModes|SlowResult)Integration$' -v ./internal/storage`，验证扫描模式及慢速接收。

Docker 清理回归：`PROJECT_ALPHA_TEST_OVERLAY_CLEANUP=1 python3 tests/test_cleanup_overlay.py` 使用本地 `python:3.12-slim`（可用 `PROJECT_ALPHA_CLEANUP_TEST_IMAGE` 指定含 Python 3 的镜像）创建并自动移除专用测试容器和镜像，不拉取镜像、不操作已有业务容器。验证真实 `docker exec` 清理、whiteout、空间释放、重复清理、socket 与字符设备保留，以及停止、只读、嵌套挂载、符号链接保护和断线取消。

存储性能基准：`go test ./internal/storage -run '^$' -bench '^(BenchmarkIncrementalScan|BenchmarkDirectoryMerge|BenchmarkSnapshotPrepare)$' -benchmem`。比较结果时使用相同的迭代数和 CPU 配置。

测试数据位于 `tests/fixtures/snapshot.json`，通过 `go run ./examples --output tests/fixtures/snapshot.json` 重新生成。


## 代码结构

- `cmd/project-alpha`：程序入口，处理进程信号并启动应用。
- `cmd/rootless-docker`、`internal/rootless`：独立 rootless Docker 管理命令、socket 热挂载和交互测试容器；不接入 Web。
- `internal/app`：命令分发、模块装配和 HTTP 服务生命周期。
- `internal/cluster`：节点注册与身份核验、总控代理、权限授权、跨节点容器统计和 API-only worker 入口。
- `internal/registry`：公网注册入口、control 出站长连接、持久身份绑定、注册会话与实时进度。
- `internal/platform`：平台 HTTP 入口、账号与会话、访问校验、审计和数据库基础；不依赖存储模块。
- `internal/storage`：扫描配置、Docker 发现与辅助扫描、任务调度、快照与增量更新、共享记录读取与探索服务，以及对应 API 和数据表。
- `internal/agent`：模型配置与凭据、Responses/Chat Completions 适配、分析会话、工具定义与编排，以及独立 API 和数据表。
- `internal/bastion`、`internal/tailscale`：共享访问资源池、Tailscale 凭据及邀请、固定 alpha-jump 跳板公钥和回收记录。
- `internal/members`：独立的机器使用者、注册 schema、邀请码页面管理和公开注册 API。
- `internal/containers`：命令行扫描导入、创建配置、启停与删除，以及管理记录和审计。
- `internal/process`：订阅 Tetragon 进程事件，常驻维护并按容器导出活动进程森林。
- `internal/httpapi`、`internal/fsutil`：共用的 HTTP/JSON 处理与路径规范化。
- `dist`：网页资源及 Go 嵌入声明；`tests`：前端回归和共享测试数据。Go 测试与所属包放在一起。
