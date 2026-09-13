# project alpha

单宿主机管理平台，当前提供存储管理模块。自动发现 Docker 容器，按容器和用户汇总存储占用，支持目录下钻、按需扫描、任务管理和 Agent 分析 API。后端使用 Go 和 SQLite，网页资源嵌入二进制。

## 代码结构

- `cmd/project-alpha`：程序入口，处理进程信号并启动应用。
- `internal/app`：命令分发、模块装配和 HTTP 服务生命周期。
- `internal/platform`：平台 HTTP 入口、账号与会话、访问校验、审计和数据库基础；不依赖存储模块。
- `internal/storage`：扫描配置、Docker 发现与辅助扫描、任务调度、快照与增量更新、磁盘分析 Agent，以及对应 API 和数据表。
- `internal/httpapi`、`internal/fsutil`：共用的 HTTP/JSON 处理与路径规范化。
- `dist`：网页资源及 Go 嵌入声明；`tests`：前端回归和共享测试数据。Go 测试与所属包放在一起。

应用层装配平台和存储模块；平台完成会话与请求校验后，通过 `platform.Module` 分发业务请求。平台表和存储表由各自维护的 `schema.sql` 定义，在同一事务中初始化。后续业务模块从应用层接入。

## 启动

要求 Linux、Go 1.24+ 和 GCC；Docker 扫描需要本机 Docker CLI 及 daemon 访问权限。

```bash
go build -o bin/project-alpha ./cmd/project-alpha
./bin/project-alpha
```

打开 <http://127.0.0.1:8765> 创建管理员，在网页配置扫描目录并开始扫描。只读账号可以查看整台主机的扫描结果。

## 配置与数据

- 数据目录默认 `data/`，通过 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR` 指定。
- Docker 归属标签默认 `project-alpha.owner`，可在扫描配置中修改，也可在网页设置容器归属。
- 扫描方式：`host` 使用服务自身权限，`docker` 使用只读辅助容器；默认 `auto` 在启用 Docker 的普通用户下优先使用辅助容器，否则直接扫描。自动模式仅在辅助容器尚未开始扫描时允许回退。
- 辅助容器默认使用本机已有的 `ubuntu:latest`，通过 `PROJECT_ALPHA_SCAN_HELPER_IMAGE` 指定含 `/usr/sbin/chroot` 的镜像，不自动拉取。服务与 Docker daemon 必须共享宿主机路径视图。
- 扫描模式：默认 `normal` 最多使用 4 路 Go 并发，`fast` 使用进程可见的全部逻辑 CPU。目录遍历为串行，速度仍受磁盘 I/O 限制。

SQLite 保存账号、配置和任务；扫描结果保存在 `data/results/`。目录增量更新按节点写入 SQLite，取消时保留已提交的明细。历史记录可在网页删除；备份时停止服务并复制整个数据目录。

1.0 发布前不保证任何前向或后向兼容性，包括数据库表结构、配置、API 和快照格式；不维护旧格式迁移或兼容分支。当前数据库格式为 v8、快照为 v2；格式不匹配时使用新的数据目录，重新配置并扫描。程序不会自动删除已有数据。

独立扫描示例：

```bash
./bin/project-alpha scan --no-docker --root /srv/models --output snapshots/latest.json
./bin/project-alpha scan --help
```

CLI 与 API 使用同一套配置校验：`max_depth` 为 0–32，`max_nodes` 为 100–100000，`docker_timeout` 为 5–3600 秒；扫描及排除目录必须为绝对路径。

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

浏览器回归：安装 Playwright 和 Chromium 后运行 `python3 tests/test_live_map_browser.py`。具备 Docker 权限和辅助镜像时，可运行 `PROJECT_ALPHA_TEST_DOCKER_MODES=1 go test -run '^TestDockerHelper(ScanModes|SlowResult)Integration$' -v ./internal/storage`，验证扫描模式及慢速接收时的结果完整性。

测试数据位于 `tests/fixtures/snapshot.json`，通过 `go run ./examples --output tests/fixtures/snapshot.json` 重新生成。

## 参考

- [存储统计口径](docs/accounting.md)
- [Agent API](docs/agent.md)
