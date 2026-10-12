# Agno 运维助理

总控首页和导航中的“运维助理”提供跨节点对话，复用“Agent 设置”里的协议、Endpoint、模型、API Key、请求超时和 Responses 思考摘要配置。现有空间分析继续使用原报告流程。

初版支持节点列表、节点概况、存储扫描状态、受管容器、容器进程和 GPU 查询，以及诊断建议。内置工具通过总控现有认证链读取 worker，不执行 shell、SSH 命令、重启或删除。管理员可以额外配置远程 MCP 工具，其能力取决于所选服务与工具。节点列表表示已登记节点；实际可达性和运行状态需进一步查询，存储数据沿用最近扫描时间。新增运维工具在 `internal/cluster/assistant.go` 注册固定的只读接口，并在 `runtime/agno/runtime.py` 暴露有明确参数的函数。

## 提示词与工具配置

在“运维助理”页展开 **助理配置 · 提示词与工具**：

- **助理提示词**：编辑巡检顺序、关注事项和回复风格，最多 16000 字符。新安装与数据库升级提供原有巡检提示词作为默认值。工具数据不能覆盖指令、不得索取凭据等服务规则独立保留。
- **内置巡检工具**：逐项启用或禁用节点列表、概况、存储、容器、进程与 GPU 查询。总控会再次校验每次工具调用。
- **MCP 服务**：配置唯一标识、Streamable HTTP 地址、Bearer Token、启用开关和明确允许的原始工具名（逗号分隔）。最多 8 个服务、每个最多 32 个工具。初版不支持 stdio、本地启动命令、旧 SSE 传输或 OAuth 登录。

配置为总控全局配置，仅管理员可查看修改；保存后从下一轮生效，进行中的请求继续使用启动时的配置。并发修改返回冲突，重新载入后再编辑。模型 Endpoint 和 API Key 继续沿用 Agent 设置；会话与长期记忆仍按账号隔离。

MCP 地址由 Agno 容器访问，可以使用内网或主机回环地址。Token 加密保存在总控数据库，页面只返回“是否已保存”；留空保留，勾选清除或移除服务会删除对应凭据。变更地址会清除原 Token，需要时重新填写。数据目录备份必须包括 `assistant-mcp.key`。

MCP 通过 SDK 初始化和发现工具，只向模型提供选中的名称（运行时加 `mcp_<服务标识>_` 前缀）；配置名称缺失、连接或认证失败会明确报错，不自动放开其他工具。禁用服务不会连接。每次实际工具调用前仍向总控校验当前管理员权限。MCP 请求不跟随重定向，不继承环境 HTTP 代理；调用超时为 60 秒，整轮仍受 15 分钟上限约束。

外部工具可能修改远端系统。只配置需要的工具；对话取消或失败不能回滚已经执行的外部操作，需在目标系统核实，避免盲目重试。健康检查只检查 Agno 服务，不连接 MCP 或模型。

## 总控容器服务

Agno 作为总控主机上的独立容器运行，部署文件为 `deploy/control-services.yaml`，镜像定义为 `deploy/agno.Dockerfile`。Python 及固定版本依赖都在镜像内，总控宿主机无需安装 Python；worker、registry 和 share node 不部署 Agno。

首次部署由部署者准备 socket 目录，归属必须与总控服务账号一致：

```sh
sudo install -d -m 0700 -o project-alpha -g project-alpha /var/lib/project-alpha-control/agno
cd /opt/project-alpha
AGNO_UID=$(id -u project-alpha) AGNO_GID=$(id -g project-alpha) \
  docker compose -f deploy/control-services.yaml up -d --build
```

需要 Docker Compose v2。执行 Compose 的部署账号应已有 Docker 管理权限。容器日常运行使用总控普通账号的 UID/GID，根文件系统只读，丢弃全部 capabilities，不挂载平台数据库或 Docker socket，不创建任何测试密钥。

默认仅把 `/var/lib/project-alpha-control/agno` 挂入容器的 `/run/agno`。容器通过 `agno.sock` 提供私有双向 JSON 通道，socket 权限为 0600；它不监听 TCP 端口。总控继续完成鉴权、读取当前 Agent 配置、调用节点工具及保存会话和记忆，容器仅执行 Agno 推理。容器使用主机网络，以保留现有 Agent Endpoint 的含义，包括 `127.0.0.1` 上的模型网关。

总控默认连接 `<data-dir>/agno/agno.sock`，可通过绝对路径 `ALPHA_AGNO_SOCKET` 覆盖。部署模板使用：

```ini
[Service]
Environment=ALPHA_AGNO_SOCKET=/var/lib/project-alpha-control/agno/agno.sock
ExecStartPre=/usr/bin/mkdir -p /var/lib/project-alpha-control/agno
```

自定义数据目录时，同时调整总控 socket 路径和 Compose 的 `AGNO_SOCKET_DIR`。例如开发环境以当前普通账号运行总控：

```sh
mkdir -p -m 0700 data-control/agno
AGNO_UID=$(id -u) AGNO_GID=$(id -g) AGNO_SOCKET_DIR="$PWD/data-control/agno" \
  docker compose -f deploy/control-services.yaml up -d --build
./bin/project-alpha --control --data-dir data-control --port 8765
```

已有 worker 数据目录不可用作总控目录。也可以直接把 socket 放入已有总控数据目录的 `agno` 子目录。

更新发布包后，以同样环境变量执行 `docker compose -f deploy/control-services.yaml up -d --build` 更新容器。updater 仍按现有流程更新总控程序和数据库，Agno 镜像需使用对应发布包重建；总控会拒绝不匹配的服务协议或依赖版本。停止使用同样环境变量执行 `docker compose -f deploy/control-services.yaml down`，不会删除总控会话与记忆。生产部署不使用宿主机 Python 回退路径。

每轮请求由容器启动隔离的 Agno 子进程，任务完成后退出。总控关闭、取消请求或连接断开会终止对应任务，容器服务保持可用；容器关闭会终止全部任务。socket 所在目录有独占锁，重启只清理本账号拥有的失效 socket，不覆盖其他文件或活动服务。

模型凭据仅通过私有 socket 传入任务，不写入容器环境、命令参数、运行文件、会话、记忆或前端响应。节点令牌始终由 Go 持有。运行任务不继承环境中的模型凭据与 HTTP 代理；需要模型代理时在 Agent Endpoint 中设置可访问的网关地址。

## 健康状态

管理员可在总控首页查看 Agno 状态，也可在运维助理页面点击“检查服务健康”。首页复用现有的总控概览刷新，助理页面在打开及手动检查时读取；不增加独立定时器或 Docker HEALTHCHECK。

健康检查验证 socket 可达、运行依赖、服务协议与版本，展示当前任务数。服务不可达、正在关闭或版本不匹配时分别给出明确状态；检查不调用模型，因此“服务就绪”不表示模型 Endpoint、API Key 或模型额度已验证。实际对话失败仍使用独立的错误提示。容器不可用时，已有会话和记忆仍可查看。

初版每轮返回完整回复，不逐字流式显示，支持停止。单次模型请求使用 Agent 设置的超时，整轮上限 15 分钟；服务端最多同时执行 4 个管理员请求，同一管理员串行执行，防止跨会话记忆覆盖。若经过反向代理，应为 `/api/assistant/` 设置至少 16 分钟的读取超时，参考 `deploy/nginx.conf.example`。连接断开时本轮通常取消，但与提交同时发生时可能已经保存，可从历史会话核对；不自动重发模型请求。

## 会话与记忆

- 会话历史按管理员账号和会话 ID 隔离，最近 10 轮进入模型上下文；历史列表展示最近 100 个会话。
- Agno 的 `enable_agentic_memory` 让助理按需通过 MemoryManager 保存、修正长期偏好与节点用途，同一管理员的不同会话共享记忆。记忆提取也复用当前 Agent 模型配置，可能产生额外模型请求。
- 对话与长期记忆都由总控存入 `platform.sqlite3`。Python 每轮使用 `InMemoryDb` 恢复该会话和用户记忆，成功后整体返回；Go 在同一事务内提交会话、记忆与审计。运行或提交失败保留此前数据。
- 记忆面板可查看和清空本人的长期记忆。清空不删除历史对话；需要排除旧对话上下文时新建会话。长期记忆不是实时状态证据。
- 运维提示要求不记录凭据，但这不是对用户输入的秘密检测器。聊天输入与模型选择记住的内容会保存；不要在对话中提交密码或密钥。

数据库初始化和 updater 使用同一份 schema 与迁移链，仅 control 新增 `assistant_sessions`、`assistant_memories` 和 `assistant_settings`。现有账号、Agent 配置、节点和报告原地保留。升级目标见 `alpha-updater --help`，支持窗口见构建生成的 `internal/platform/upgrade_history.json`，按 [updater 说明](updater.md) 执行。窗口外数据库明确报错并保留数据。

Agno 状态带格式标识及固定依赖版本。格式不匹配明确报错，不覆盖已有状态；非数据库状态格式不兼容时使用新数据目录。后续若改变数据库结构，必须同步更新初始化 schema、DatabaseVersion、共享迁移链和升级窗口。

## HTTP 接口

接口仅位于总控，要求管理员会话；变更请求必须携带现有 `X-CSRF-Token`。不接受客户端提供的 user_id、模型配置或工具路径。

| 接口 | 作用 |
|---|---|
| `GET /api/assistant/settings` | 读取共享助理配置、版本、内置工具目录和凭据存在标记 |
| `PUT /api/assistant/settings` | 保存完整配置及版本；`tokens` 为仅写入的服务标识到 Token 映射，`clear_tokens` 指定清除项 |
| `GET /api/assistant/health` | 本机 Agno 服务状态、版本、运行任务数及检查时间；异常状态也返回 200，状态见 `status` |
| `GET /api/assistant/sessions` | 本人的最近 100 个会话 |
| `POST /api/assistant/sessions` | `{"message":"问题"}`，执行并保存新对话，返回 200 |
| `GET /api/assistant/sessions/:id` | 本人的会话及已完成消息 |
| `POST /api/assistant/sessions/:id/messages` | `{"message":"追问"}`，执行并保存下一轮，返回 200 |
| `GET /api/assistant/memories` | 本人的长期记忆 |
| `DELETE /api/assistant/memories` | 清空本人的长期记忆，运行中返回 409 |

## 验证

开发测试可用 Python 3.12+ 虚拟环境运行容器服务代码，生产总控仍只连接 socket：

```sh
python3.12 -m venv agno-venv
agno-venv/bin/python -m pip install -r runtime/agno/mcp-requirements.txt
go generate ./internal/platform
ALPHA_AGNO_TEST_PYTHON="$PWD/agno-venv/bin/python" go test ./internal/assistant ./internal/cluster ./internal/updater ./cmd/alpha-updater
PYTHONDONTWRITEBYTECODE=1 agno-venv/bin/python -m unittest discover -s tests -p test_agno_runtime.py

docker build -t project-alpha-agno:test -f deploy/agno.Dockerfile .
ALPHA_AGNO_TEST_IMAGE=project-alpha-agno:test python3 tests/test_agno_container.py
```

Python 测试使用真实固定版本 Agno 与 OpenAI SDK，通过本地模拟响应验证模型协议、工具循环、记忆写入和跨会话恢复。Go 集成测试覆盖权限、会话隔离、提交回滚、socket 桥接、健康状态、取消请求和真实 updater 的两个迁移入口，不调用付费模型。容器测试使用独立 Compose 项目和临时数据目录，覆盖非 root 运行、实际模型工具循环、记忆持久化、重启、取消及离线恢复，结束后清理测试容器。

本次新增 MCP 依赖与通道版本，升级总控后需用对应发布包重建 Agno 容器。已有会话状态格式保持不变。

Agno 官方参考：[MCP Streamable HTTP](https://docs.agno.com/tools/mcp/transports/streamable_http)、[会话与记忆](https://docs.agno.com/use-cases/product-agents/sessions-and-memory)、[Agent Memory](https://docs.agno.com/memory/agent/overview)。
