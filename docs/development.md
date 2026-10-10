# 开发与验证

项目使用 Go 和 SQLite，网页资源直接嵌入二进制。修改 `dist/` 后重新构建即可，无需单独打包前端。

## 构建版本

`project-alpha --version` 显示二进制版本和构建信息。普通源码构建的版本为 `dev`；Go 可用的 VCS 信息会带上提交及工作区修改标记。发布工作流在构建时写入 tag（分支构建使用 `dev-<提交>`）和完整提交 ID，使输出与包内 `BUILD_INFO` 一致。

构建 updater 或发布包前，在拥有完整 Git 历史与 tags 的检出中运行 `go generate ./internal/platform`，生成最近 3 次有数据库变更的 tag 升级窗口。无数据库变更的 tag 和未打 tag 的开发提交不占名额。新增迁移需登记版本步骤，见 [升级保留规则](updater.md#按-tag-保留最近-3-次数据库更新)。发布工作流自动生成并验证此窗口。

发布工作流在两个架构的 Ubuntu 20.04 构建容器中，逐个为 `ctools/*/Makefile` 执行 `make release`。新增工具须提供该目标，接受 `BUILD`、`RELEASE_DIR`、`RELEASE_NAME`，生成 `$(RELEASE_DIR)/$(RELEASE_NAME).tar.gz`，包内顶层目录与 `RELEASE_NAME` 同名。工作流将包命名为 `ctools-<工具名>_<版本>_linux_<架构>.tar.gz`，检查 `bin/*` 和 `lib/*.so*` 的 glibc 要求，并执行包内可执行文件的 `--help`。分支构建和发布均包含独立工具包及其校验和。

自定义构建版本示例：

```bash
go build -o bin/project-alpha \
  -ldflags '-X project-alpha/internal/buildinfo.Version=v0.3.1' \
  ./cmd/project-alpha
./bin/project-alpha --version
```

## 验证

前端测试要求 Node.js 20+，无 npm 依赖。

```bash
go test -race ./...
go vet ./...
for test in tests/test_*.js; do node "$test" || exit; done
make -C ctools/dram-bw -j test
```

`ctools/dram-bw` 使用 C11 编译器、Linux 开发头文件、make 和 Python 3，独立构建和测试。上述测试使用 mock 后端与临时 Unix socket，无需 PMU 权限；CI 同样运行此检查。性能基准和可选硬件对照见 [dram-bw 验证说明](../ctools/dram-bw/docs/validation.md)。可从源码单独安装，或使用 Release 中独立的 `ctools-dram-bw_*.tar.gz`。

浏览器回归需要 Playwright 和 Chromium：

```bash
python3 tests/test_members_browser.py
python3 tests/test_registry_browser.py
python3 tests/test_registration_choices_browser.py
python3 tests/test_cluster_browser.py
python3 tests/test_scan_schedule_browser.py
python3 tests/test_status_browser.py
```

测试使用临时数据及模拟模型、Tailscale 或资源响应。其余浏览器测试在 `tests/test_*_browser.py`，覆盖工作台、登录、Agent、目录探索和清理。

`python3 tests/test_bastion_ssh.py` 在临时容器内验证真实 SSH、两个账号的权限分离、HTTP 代理、重复初始化和卸载，不连接外部服务。默认使用已有 `sc2025:1.0` 镜像，可用 `PROJECT_ALPHA_BASTION_IMAGE` 指定含 OpenSSH、Python 3 和用户管理工具的镜像；不拉取镜像，不修改宿主账号或 SSH。

SSH 测试不生成密钥。集成测试需用 `PROJECT_ALPHA_BASTION_KEY_DIR` 指定已有的专用测试密钥目录，包含无密码的 `control_ed25519`、`member_ed25519`、`free_ed25519`、`host_ed25519` 及各自的 `.pub` 文件；未指定时跳过。Go 的 sshd 配置测试用 `PROJECT_ALPHA_SSH_HOST_KEY` 指定已有测试主机私钥，未指定时跳过。

具备 Docker 权限和辅助镜像时，可运行 `PROJECT_ALPHA_TEST_DOCKER_MODES=1 go test -run '^TestDockerHelper(ScanModes|SlowResult)Integration$' -v ./internal/storage`，验证扫描模式及慢速接收。

Docker 清理回归：`PROJECT_ALPHA_TEST_OVERLAY_CLEANUP=1 python3 tests/test_cleanup_overlay.py` 使用本地 `python:3.12-slim`（可用 `PROJECT_ALPHA_CLEANUP_TEST_IMAGE` 指定含 Python 3 的镜像）创建并自动移除专用测试容器和镜像，不拉取镜像、不操作已有业务容器。验证真实 `docker exec` 清理、whiteout、空间释放、重复清理、socket 与字符设备保留，以及停止、只读、嵌套挂载、符号链接保护和断线取消。

存储性能基准：`go test ./internal/storage -run '^$' -bench '^(BenchmarkIncrementalScan|BenchmarkDirectoryMerge|BenchmarkSnapshotPrepare)$' -benchmem`。比较结果时使用相同的迭代数和 CPU 配置。

测试数据位于 `tests/fixtures/snapshot.json`，通过 `go run ./examples --output tests/fixtures/snapshot.json` 重新生成。

## 代码结构

- `cmd/project-alpha`：程序入口，处理进程信号并启动应用。
- `cmd/alpha-updater`、`internal/updater`：独立测试环境更新器；按角色校验并安装 GitHub release，调用目标更新器原地升级数据库。用法见 [测试环境更新器](updater.md)。
- `cmd/rootless-docker`、`internal/rootless`：rootless Docker daemon/client、host 用户服务生命周期监督、socket 挂载/卸载及事件恢复；部署见 [rootless Docker](rootless-docker.md)。
- `ctools/dram-bw`：独立 C DRAM 带宽采集服务、客户端库、命令行示例和 perf 对照脚本；用法见 [dram-bw](../ctools/dram-bw/README.md)。
- `internal/app`：命令分发、模块装配和 HTTP 服务生命周期。
- `internal/cluster`：节点注册与身份核验、总控代理、权限授权、跨节点容器统计和 API-only worker 入口。
- `internal/registry`：公网注册入口、control 出站长连接、持久身份绑定、注册会话与实时进度。
- `internal/platform`：平台 HTTP 入口、账号与会话、访问校验、审计和数据库基础；不依赖存储模块。
- `internal/storage`：扫描配置、Docker 发现与辅助扫描、任务调度、快照与增量更新、共享记录读取与探索服务，以及对应 API 和数据表。
- `internal/agent`：模型配置与凭据、Responses/Chat Completions 适配、分析会话、工具定义与编排，以及独立 API 和数据表。
- `internal/bastion`、`internal/tailscale`：共享访问资源池、Tailscale 凭据及邀请、alpha-worker 远程公钥管理、alpha-jump 成员转发及 share node HTTP 代理。
- `internal/members`：独立的机器使用者、注册 schema、邀请码页面管理和公开注册 API。
- `internal/containers`：命令行扫描导入、创建配置、启停与删除，以及管理记录和审计。
- `internal/gpu`：NVIDIA GPU 采集、容器用户归属、72 小时滚动历史与时间聚合。
- `internal/mihomo`：订阅解析、过滤与模板生成、继承配置及持久下发、三个角色共用的核心进程托管和策略组选择；见 [代理管理](mihomo.md)。
- `internal/process`：订阅 Tetragon 进程事件，常驻维护并按容器导出活动进程森林。
- `internal/httpapi`、`internal/fsutil`：共用的 HTTP/JSON 处理与路径规范化。
- `dist`：网页资源及 Go 嵌入声明；`tests`：前端回归和共享测试数据。Go 测试与所属包放在一起。
