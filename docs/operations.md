# 运行与配置

首次启动和节点接入见 [快速开始](README_cn.md#快速开始)。这里补充公网注册、数据保存、独立扫描和服务部署的说明。

部署成员跳板和总控网页代理见 [share node 配置指南](share-node.md)。

## 命令行帮助与版本

主帮助列出子命令和常用参数；每个子命令支持 `--help` 或 `-h`，查看完整参数及示例。

```bash
./bin/project-alpha --help
./bin/project-alpha serve --help
./bin/project-alpha share-node --help
./bin/project-alpha containers import --help
./bin/project-alpha scan --help
./bin/project-alpha process --help
./bin/project-alpha --version
```

`--version` 输出版本、构建提交、Go 版本及平台。发布包的版本与 tag 一致；普通源码构建显示 `dev`，可用构建时的 `-ldflags -X` 指定，见 [开发说明](development.md#构建版本)。帮助和版本查询不会启动服务、初始化账号或创建数据目录。

## 公网 registry

公网注册使用 registry，总控主动建立出站连接：

```bash
REG_PASS=Ab3dE6gH ./bin/project-alpha --registry --data-dir ./registry-data --port 8767 \
  --allowed-host register.example.com --secure-cookie
```

在总控添加 registry 连接，并配置邀请码和使用者资源。注册入口为 `https://register.example.com/registry/Ab3dE6gH/<邀请码>`。部署参考 [registry systemd 单元](../deploy/project-alpha-registry.service) 和 [nginx 示例](../deploy/nginx-registry.conf.example)，登记规则见 [使用者登记](members.md)。

registry 启动时生成并打印供总控连接的令牌，保存到数据目录的 `registry-token`，重启后复用。也可通过 `--registry-token-file` 或 `PROJECT_ALPHA_REGISTRY_TOKEN` 指定；显式令牌不打印。此连接令牌与注册入口的 `REG_PASS` 分别使用。

## 配置与数据

总控、worker 和 registry 各使用独立数据目录，目录绑定运行角色。默认目录为 `data/`，可通过 `--data-dir` 或 `PROJECT_ALPHA_DATA_DIR` 指定。未指定服务模式时启动总控；`--control`、`--worker` 和 `--registry` 互斥。

数据库格式为 v34，快照格式为 v5。1.0 发布前不保证格式兼容；数据库提供保留数据的原地升级，当前支持从 v0.3.1 的数据库 v33 升级到 v34，见 [测试环境更新器](updater.md)。其他旧格式不匹配时使用新数据目录，程序保留已有数据。

总控保存账号、使用者、节点连接、Agent 配置、会话、报告和清理记录；worker 保存扫描配置、任务、容器和审计。扫描结果位于 `results/`。Agent API Key 加密存入 SQLite，密钥位于总控数据目录的 `agent-api-key.key`。备份时停止服务，复制整个数据目录及密钥文件。

扫描后端 `host` 使用服务账号权限，`docker` 使用只读辅助容器；默认 `auto` 在启用 Docker 的普通账号下使用辅助容器，否则直接扫描。辅助容器使用 `alpine:latest`，可通过 `PROJECT_ALPHA_SCAN_HELPER_IMAGE` 指定；本地缺少时拉取，启动失败时任务报错。扫描模式 `normal` 最多使用 4 路并发，`fast` 使用进程可见的全部逻辑 CPU。

独立扫描：

```bash
./bin/project-alpha scan --no-docker --root /srv/models --output snapshots/latest.json
./bin/project-alpha scan --help
```

统计定义见 [存储统计口径](accounting.md)，快照和目录探索见 [记录读取与探索](records.md)。

管理员可在空间用量页生成 Host 或容器报告，在“诊断清理”读取报告条目并选择清理范围。每批删除需输入所选 worker 系统服务账号的 sudo 密码；处理结果持久保存，清理后重新扫描更新用量。报告、权限与接口见 [Agent API](agent.md)。

## 进程监控

worker 通过 Tetragon 事件和进程缓存维护活动进程树，默认 socket 为 `/var/run/tetragon/tetragon.sock`，可用 `--tetragon-socket` 配置。节点接口 `GET /api/process/forest?host=1` 返回连接状态和进程树；未连接时返回 503。

每 30 秒校准缓存，连续两次缺失且无法从宿主机 `/proc` 确认仍在运行的节点会从显示中移除。缓存淘汰、丢失事件、不可读的 procfs 或 PID 命名空间不一致都可能导致显示遗漏。校准只维护显示数据。

```bash
./bin/project-alpha process --duration 30s --output process-forest.json
./bin/project-alpha process --events-file tests/fixtures/tetragon-events.jsonl --output process-forest.json
```

## 部署

将二进制放到 `/opt/project-alpha/bin/project-alpha`，调整 [总控 systemd 单元](../deploy/project-alpha.service) 的 `User=` 为实际普通服务账号，再安装并启动。每台节点使用 [worker 单元](../deploy/project-alpha-worker.service)，将 `User=` / `Group=` 改为实际普通服务账号，并准备由该账号持有、权限为 0600 的 `/etc/project-alpha/node-token`。worker 模板默认账号名为 `project-alpha`，需提前创建或替换，不使用 share node 的 `alpha-worker`。HTTPS 部署参考 [nginx 示例](../deploy/nginx.conf.example)，配置 `--allowed-host` 和 `--secure-cookie`。

节点容器页面提供 [权限检查和 sudo 修复](containers.md#节点权限检查与修复)。使用网页修复时，worker 账号须有相应 sudo 执行权限，节点安装 `sudo`、`acl`（提供 `setfacl`）及 `groupadd` / `usermod`。worker 单元需允许 sudo 提权（`NoNewPrivileges=false`，且 sudo 策略不要求终端）；模板已设置。已部署的单元需自行同步并运行 `sudo systemctl daemon-reload`、`sudo systemctl restart project-alpha-worker`。本功能不修改 sudoers、不自动重启服务；加组后也须重启 worker 才能更新进程组列表。手动从终端启动的 worker 需在重新登录后的会话中启动。

总控默认监听回环地址，也可使用 SSH 转发访问：

```bash
ssh -L 8765:127.0.0.1:8765 user@your-server
```
