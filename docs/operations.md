# 运行与配置

首次启动和节点接入见 [快速开始](README_cn.md#快速开始)。这里补充公网注册、数据保存、独立扫描和服务部署的说明。

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

数据库格式为 v25，快照格式为 v5。1.0 发布前不保证格式兼容，不提供旧格式迁移；格式不匹配时使用新数据目录，程序保留已有数据。

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

将二进制放到 `/opt/project-alpha/bin/project-alpha`，调整 [总控 systemd 单元](../deploy/project-alpha.service) 的 `User=` 为实际普通服务账号，再安装并启动。每台节点使用 [worker 单元](../deploy/project-alpha-worker.service)，准备权限为 0600 的 `/etc/project-alpha/node-token`。HTTPS 部署参考 [nginx 示例](../deploy/nginx.conf.example)，配置 `--allowed-host` 和 `--secure-cookie`。

总控默认监听回环地址，也可使用 SSH 转发访问：

```bash
ssh -L 8765:127.0.0.1:8765 user@your-server
```
