# 容器管理

node 通过本机 Docker CLI 执行容器管理。所有网页位于总控，选择节点后使用 `/api/cluster/nodes/<节点ID>/api/containers...` 代理下述接口；node 不提供网页或独立账号。默认 endpoint 为 `unix:///var/run/docker.sock`；显式传入 `--host`，忽略服务环境中的 `DOCKER_CONTEXT` / `DOCKER_HOST`，避免运行期间切换到其他 daemon。管理功能不依赖存储扫描或 Tetragon。服务账号必须有 Docker 权限，并与 daemon 共享宿主机文件路径视图。

## 命令行导入

```bash
./project-alpha containers import --data-dir ./node-data
./project-alpha containers import --data-dir ./node-data --dry-run
./project-alpha containers import --data-dir ./node-data --endpoint unix:///var/run/docker.sock --base-dir /docker alice bob
```

导入必须在对应 node 上执行，并使用该 node 的 worker 数据目录；总控数据目录会被拒绝。导入直接读取 Docker 并登记到 `platform.sqlite3`，不需要 Web 服务或平台账号。省略容器名时使用 `docker ps -a --no-trunc --quiet` 扫描全部容器，也可在所有选项之后指定名称或完整 ID。`--data-dir` 默认取 `PROJECT_ALPHA_DATA_DIR`，否则为 `data`。`--endpoint` / `--base-dir` 默认读取已有配置；新目录默认使用本机 Docker socket 和 `/docker`。

通过检查的容器直接登记，所属用户默认为容器名，归属覆盖和审计与记录一起提交。显式传入的 endpoint、base_dir 在成功登记时保存；已有管理记录时不允许切换 endpoint。重复执行跳过已登记项，不覆盖记录和归属。同名替换容器必须先解除旧记录。每项输出结果和失败原因；失败项不阻止其他有效项登记，存在失败项时返回非零退出码，成功记录保留。数据库写入失败或取消会停止本次导入，此前成功记录保留。

`--dry-run` 只报告可导入项、已登记项和失败项，不写入管理记录、归属、配置或审计；若数据目录是新的，仍会初始化平台数据库。格式不符要求使用新数据目录，不自动迁移或清理。建议停止 node 后导入，再使用相同数据目录和 `--worker` 启动。导入成功后停止使用原 Compose 文件操作已导入容器。

网页保留创建和日常管理；不提供扫描接管表单、检查 API 或接管 API。

## API

所有接口要求登录，所有写操作要求管理员及平台 CSRF 校验。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/containers` | 管理记录和实时状态；连接故障在 `error` 中显示 |
| GET / PUT | `/api/containers/settings` | 管理员读取/保存 endpoint、image、base_dir、start_port、ssh_host、proxy_jump |
| POST | `/api/containers` | 创建并初始化；返回 ID、名称、SSH 端口及一次性展示的密码 |
| POST | `/api/containers/<完整ID>/start` | 启动 |
| POST | `/api/containers/<完整ID>/stop` | 停止，10 秒宽限期 |
| POST | `/api/containers/<完整ID>/restart` | 重启，10 秒宽限期 |
| POST | `/api/containers/<完整ID>/initialize` | 重试未完成初始化的容器；可指定 password，否则随机生成 |
| POST | `/api/containers/<完整ID>/delete` | `{confirm: 完整容器名}`；删除已停止的容器，保留挂载数据 |
| POST | `/api/containers/<完整ID>/release` | `{confirm: 完整容器名}`；解除平台管理，不调用 Docker |

创建请求：

```json
{
  "name": "alice",
  "owner": "alice",
  "image": "training:latest",
  "network": "bridge",
  "port": 0,
  "gpus": "all",
  "local_proxy": false,
  "password": ""
}
```

owner 必须填写总控已登记的集群使用者 `username`，image 默认使用配置镜像，network 默认 bridge，gpus 默认 all。port 为 0 时自动分配，password 为空时使用密码学随机值。密码不能包含换行、冒号或 NUL。容器名称不能为共享目录名 `data`。没有请求参数的写操作发送 `{}`。

## 检查与身份约束

接管检查只读宿主机路径、Docker inspect 和容器中的 `sshd -T`，不启动/停止/修改容器。必须正常运行才能验证 SSH；启动失败的旧容器先用原 Docker 环境修复。bridge 要求有效 SSH 端口为 22，且 `22/tcp` 映射到单一有效宿主机端口（允许 IPv4 / IPv6 使用相同端口）；host 要求有效 SSH 端口唯一且没有 Docker 端口发布配置。检查不尝试读取、重置原 root 密码，也不验证外部 SSH 登录。

只接受原脚本的三项直接、可写 bind 挂载，不接受额外挂载或符号链接路径。GPU 请求需为 NVIDIA all 或 1–7 张；设备 ID 选择不在当前模板范围内。检查只验证声明的 GPU 请求，实际运行可用性仍由 Docker/NVIDIA 驱动负责。拒绝的每一项都含名称与原因。

管理记录保存 endpoint、daemon 身份、完整容器 ID、规格和配置摘要。摘要覆盖完整 Config、HostConfig 和 Mounts；不保存原 inspect 或启动命令，因此原脚本命令中的密码不进入平台数据库。实时状态和 IP 地址不参与摘要。每次生命周期操作核实身份及配置并使用完整 ID，不通过名称重新寻找容器。

写操作在当前服务进程内串行；并行请求收到明确的“其他操作进行中”错误。外部 Docker 客户端仍可能同时操作容器，平台无法锁住外部客户端。完整 ID 防止名称复用导致误操作；Docker 自身最终决定启停/删除结果。不要启动多个服务进程共同操作同一数据目录。

## 失败恢复

- 创建前缺少镜像、端口冲突、数据目录存在或参数错误：不创建容器，修正提示中的问题后重试。
- 准备目录或 Docker create 失败：已准备的目录保留，不自动删除数据；核对容器是否实际创建，并由管理员处理空目录或选择新名称后重试。
- 新容器已创建但 inspect/登记失败：只尝试撤销本次创建且尚未登记的容器，不强制删除，不删除数据卷。撤销失败会列出完整 ID 和原因；核对 Docker 实际状态。
- 登记后启动、密码或 SSH 初始化失败：记录显示“等待密码初始化”。修复 Docker/镜像环境后重试初始化，或者停止并删除该容器；初次成功初始化前 SSH 不会启用。
- 客户端断线或超时：Docker 可能已执行操作，先刷新。若容器存在但没有管理记录，按错误中提供的 ID 核实；本次创建的初始化等待命令可从 inspect 识别，必要时手动删除该未使用容器后重新创建。平台不清除已有目录。
- 外部修改、删除、同名替换或 daemon 身份改变：拒绝生命周期操作；管理员可解除旧记录，再通过命令行导入当前容器。
- Docker 删除成功但数据库更新失败：记录暂时保留，刷新显示容器不存在；恢复数据库后解除该记录。

删除使用 `docker rm <完整ID>`，无 `--force`、无 `--volumes`。Docker 命令参数参见 [Docker create 文档](https://docs.docker.com/reference/cli/docker/container/create/) 与 [inspect 文档](https://docs.docker.com/reference/cli/docker/container/inspect/)。

CLI 导入保留原有文本归属，未登记标识在总控统计中明确单列。

## 使用者自动分配

注册与本人补申请复用节点的默认镜像、端口和数据目录设置，以成员 ID 固定创建一份容器，注入带成员注释的 SSH 公钥。分配失败后可核对并继续原容器；删除使用者会回收其分配容器及专属数据，共享 `/data` 保留。详见 [跳板机与使用者资源](bastion.md)。
