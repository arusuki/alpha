# Agno 运维助理

总控首页和导航中的“运维助理”提供跨节点对话，复用“Agent 设置”里的协议、Endpoint、模型、API Key、请求超时和 Responses 思考摘要配置。现有空间分析继续使用原报告流程。

初版支持节点列表、节点概况、存储扫描状态、受管容器、容器进程和 GPU 查询，以及诊断建议。内置工具通过总控现有认证链读取 worker，不执行 shell、SSH 命令、重启或删除。管理员可以额外配置远程 MCP 工具，其能力取决于所选服务与工具。节点列表表示已登记节点；实际可达性和运行状态需进一步查询，存储数据沿用最近扫描时间。新增运维工具在 `internal/cluster/assistant.go` 注册固定的只读接口，并在 `runtime/agno/runtime.py` 暴露有明确参数的函数。

## 使用者 CLI

集群使用者通过独立的 `alpha-chat` 可执行文件对话，源码入口为 `cmd/alpha-chat`，不向 `project-alpha` 添加子命令。总控指挥 worker 将程序和连接配置自动装入归属于该使用者的受管容器，用户直接运行：

```sh
alpha-chat
```

在总控“运维助理”展开 **使用者 CLI · 自动下发**，管理员可设置：

- **命令完整路径**：默认 `/usr/local/bin/alpha-chat`，例如改成 `/usr/local/bin/ask` 后用户运行 `ask`。自定义目录需已在容器的 `PATH` 中，否则使用完整路径调用。
- **总控／分享入口地址**：默认留空，按使用者分配的分享节点自动生成地址。也可填写所有目标容器能访问的 HTTP(S) 总控入口。地址不含 `/status/...` 路径，使用者账号始终由总控自动填写。
- **保存并下发／重新下发**：保存会为已有使用者和 worker 排队下发；面板可刷新查看每个使用者、每个节点的成功或失败信息。并发修改配置会返回冲突，需重新载入。

注册使用者、创建／领养容器、连接／重连 worker、修改分享入口和总控重启都会触发同步。worker 持久化目标配置，在自身重启和容器启动／重启时补装；停止的容器保留失败信息，可启动后重试。没有受管容器的 worker 仅保存配置，供后续创建时安装。直接在平台外启动 Docker 容器时，管理员可点击“重新下发”。不增加独立定时健康检查。

worker 使用自己安装目录中同版本的 `alpha-chat`，发布包及 worker updater 会一起安装它，无需总控上传不同架构的二进制。容器架构须与 worker 二进制兼容，需提供 `/bin/sh`、`tar`、`sha256sum`、`cut`、`mktemp`、`readlink` 和支持 `mv -T` 的基础工具。程序为静态 Go 可执行文件，不依赖容器 glibc。源码部署应在 worker 主程序旁构建：

```sh
go generate ./internal/platform
CGO_ENABLED=0 go build -o bin/alpha-chat ./cmd/alpha-chat
```

worker 通过现有 Docker 管理权限安装到容器内部，宿主机服务仍以普通账号运行。安装路径必须位于容器自身文件系统，不能落在挂载目录或符号链接目录中。命令指向隐藏的版本目录，程序与 `alpha-chat.json` 配置一起切换；改名只清理本服务记录的旧命令，已有其他文件不会被覆盖。下发配置只含 `server` 和 `username`，不含密码或 token。

`--user` 是注册时的使用者标识，密码与 `/status/<username>` 相同，不是平台管理员密码。首次在终端隐藏输入密码，登录复用 `POST /api/status/<username>/login`，取得 12 小时有效的随机访问 token。之后 HTTP 请求使用 `Authorization: Bearer <token>`，服务端根据 token 查找身份，不接受模型或客户端指定身份。请通过已有的 Tailscale 私网分享入口或 HTTPS 总控连接。

CLI 在用户配置目录的 `alpha-chat/session.json` 保存服务器地址、使用者标识和 token，文件权限为 0600；不保存密码。容器内从首次启动就无需 `--server` 和 `--user`。独立下载运行时仍可用这两个参数显式配置；参数优先于下发配置，下发配置优先于已保存的登录地址和账号。token 只复用于相同服务器和使用者；失效时重新输入密码。`--config` 可指定另一份登录文件。客户端不跟随 HTTP 重定向、不继承环境 HTTP 代理、不自动重发问题。

```sh
# 交互对话：/new 新建会话，/exit 退出；Ctrl-C 取消并退出
alpha-chat
# 一次性提问，输出回复及可用于继续对话的会话 ID
alpha-chat --message '查看我的容器状态'
alpha-chat --list
alpha-chat --session <会话ID> --message '继续检查进程'
# 注销当前 token 并删除本地登录文件
alpha-chat --logout
```

自动化调用可用 `--password-stdin` 从标准输入第一行读取密码并重新登录；密码不作为命令行参数。交互隐藏输入目前支持 Linux。整轮回复完成后显示结果，不逐字流式输出。取消或连接失败后可以用 `--list` 和 `--session` 核对历史，不自动重试。

成员请求、每次模型工具调用和提交会话事务都会重新检查 token 的有效期与成员状态；退出登录、修改／重置密码或删除成员后，旧 token 不能继续调用工具或提交本轮结果。校验发生在 Go 服务中，用户 token 不传给模型、Agno 容器或 worker；worker 继续使用总控已有的服务认证通道和服务端确认的使用者身份。

成员会话与记忆按稳定成员 ID 隔离，独立于平台账号；删除成员时级联删除对应会话和记忆。成员继承管理员启用的内置工具列表，但返回范围固定为：

- 节点列表只返回 worker 的 ID 与名称；节点概况与容器列表只返回本人容器及必要的状态信息。
- 进程树只返回当前归属于本人容器的进程；不返回宿主机或其他人的进程树，缩写容器 ID 必须唯一匹配。
- 磁盘概览和 GPU 概览复用状态页的共享可见范围，包含现有跨使用者容量和 GPU 使用统计；不开放宿主机目录树。

成员不能查看／修改助理配置或模型凭据，不能使用管理员配置的远程 MCP 服务。当前没有将成员 token 委托给外部 MCP 的机制，不能把全局 MCP Bearer Token 当作成员授权。内置工具保持只读。

成员接口位于总控，并通过分享节点的明确白名单开放。以下接口均要求使用者 Bearer token，不使用管理员 Cookie 或 CSRF token；仍校验 Host/Origin。

| 接口 | 作用 |
|---|---|
| `GET /api/member-assistant/me` | token 对应的成员 ID 和使用者标识 |
| `POST /api/member-assistant/logout` | 注销当前 token |
| `GET /api/member-assistant/sessions` | 本人的最近 100 个会话 |
| `POST /api/member-assistant/sessions` | `{"message":"问题"}`，执行并保存新对话 |
| `GET /api/member-assistant/sessions/:id` | 本人的会话及消息 |
| `POST /api/member-assistant/sessions/:id/messages` | `{"message":"追问"}`，继续对话 |
| `GET /api/member-assistant/memories` | 本人的长期记忆 |
| `DELETE /api/member-assistant/memories` | 清空本人记忆，运行中返回 409 |

共享并发上限仍为 4，同一身份串行执行；每轮上限 15 分钟。分享节点为成员助理单独设置 16 分钟响应超时，其他接口保持原超时；额外反向代理也需覆盖 `/api/member-assistant/`，参见 `deploy/nginx.conf.example`。

自动下发新增 control 的 `chat_cli_settings`、`member_cli_sync`，以及 worker 的 `member_cli_configs`、`container_cli_installs`。成员会话新增 control 专用的 `member_assistant_sessions` 和 `member_assistant_memories`，不修改现有使用者密码和 token 表。开发与发布升级均通过共享迁移链原地保留管理员对话、成员、登录会话和其他已有数据。升级目标以 `alpha-updater --help` 为准，支持窗口见生成的 `internal/platform/upgrade_history.json`。先停止使用相应目录的服务，再运行：

```sh
./bin/alpha-updater --database-only --data-dir <已有数据目录>
```

## 提示词与工具配置

在“运维助理”页展开 **助理配置 · 提示词与工具**：

- **助理提示词**：编辑巡检顺序、关注事项和回复风格，最多 16000 字符。新安装与数据库升级提供原有巡检提示词作为默认值。工具数据不能覆盖指令、不得索取凭据等服务规则独立保留。
- **内置巡检工具**：逐项启用或禁用节点列表、概况、存储、容器、进程与 GPU 查询。总控会再次校验每次工具调用。
- **MCP 服务**：配置唯一标识、Streamable HTTP 地址、Bearer Token、启用开关和明确允许的原始工具名（逗号分隔）。最多 8 个服务、每个最多 32 个工具。初版不支持 stdio、本地启动命令、旧 SSE 传输或 OAuth 登录。

配置为总控全局配置，仅管理员可查看修改；保存后从下一轮生效，进行中的请求继续使用启动时的配置。并发修改返回冲突，重新载入后再编辑。模型 Endpoint 和 API Key 继续沿用 Agent 设置；会话与长期记忆仍按账号隔离，成员使用独立身份和表。

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

初版每轮返回完整回复，不逐字流式显示，支持停止。单次模型请求使用 Agent 设置的超时，整轮上限 15 分钟；服务端最多同时执行 4 个助理请求，同一管理员或成员串行执行，防止跨会话记忆覆盖。若经过反向代理，应为 `/api/assistant/` 设置至少 16 分钟的读取超时，参考 `deploy/nginx.conf.example`。连接断开时本轮通常取消，但与提交同时发生时可能已经保存，可从历史会话核对；不自动重发模型请求。

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
| `GET /api/assistant/cli` | 读取命令路径、地址、配置版本及逐使用者／节点下发结果 |
| `PUT /api/assistant/cli` | 保存 `{revision,server,command_path}` 并排队下发，返回 202；server 可留空自动生成 |
| `POST /api/assistant/cli/sync` | 以空对象请求重新下发，返回 202 |
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

Python 测试使用真实固定版本 Agno 与 OpenAI SDK，通过本地模拟响应验证模型协议、工具循环、记忆写入和跨会话恢复。Go 集成测试覆盖密码登录与 CLI、成员 token 撤销、工具数据隔离、分享入口、会话隔离、提交回滚、socket 桥接、健康状态、取消请求和真实 updater 的两个迁移入口，不调用付费模型。容器测试使用独立 Compose 项目和临时数据目录，覆盖非 root 运行、实际模型工具循环、记忆持久化、重启、取消及离线恢复，结束后清理测试容器。

本次新增 MCP 依赖与通道版本，升级总控后需用对应发布包重建 Agno 容器。已有会话状态格式保持不变。

Agno 官方参考：[MCP Streamable HTTP](https://docs.agno.com/tools/mcp/transports/streamable_http)、[会话与记忆](https://docs.agno.com/use-cases/product-agents/sessions-and-memory)、[Agent Memory](https://docs.agno.com/memory/agent/overview)。
