# 节点服务管理

管理员从集群卡片点击“管理服务”，或进入 worker 工作台的“节点服务”页面，管理 `deploy/services.yaml` 部署的 Tetragon、DRAM 带宽与 Rootless Docker。每个服务可单独启动、停止、重启，保存自动重启策略及停止等待时间。切换服务、刷新状态和操作失败均保留尚未保存的表单内容。页面上的状态来自 Docker 容器；“运行中”并不表示所有采集功能都已就绪。Tetragon 后启动或重建 socket 时，进程监控使用现有事件连接的重连机制恢复。

接口只操作带有对应 `project-alpha.service` 标签的唯一容器，核对完整容器 ID。未部署、同名标签重复、暂停或身份不匹配时明确报错，不创建替代容器。镜像、Tetragon 采集参数及宿主机挂载仍在 Compose 文件中设置。DRAM 采集参数可以在专用配置页保存并应用。Compose 重建可能覆盖 Docker 的重启策略，之后在页面保存配置或启动服务可重新应用已保存的策略。

## DRAM 采集参数

- **采样间隔**：默认 100000 微秒，范围 100–10000000；页面同时显示对应的近似频率。间隔指每轮扫描完成后的等待时间，实际频率还受扫描耗时影响。AMD Rome 推荐至少 100 ms，较短间隔可能产生无效样本。
- **采集后端**：默认 `amd-rome`，也可选择 `auto` 自动检测或 `mock` 模拟数据。硬件不可用时明确失败，不自动退回模拟数据。
- **参考峰值带宽**：默认 0，表示不计算利用率；可配置 0–1000000 GB/s。此采集器报告宿主机带宽，不代表单个用户或容器的独占带宽。

DRAM 循环重启时，可在页面停止服务，也可直接修正参数并保存；保存会先停止重启循环，再下发配置并重新启动。停止或下发失败时保留错误，修复后重试。

保存时将参数下发到 DRAM 服务容器的 `/run/dram-bw/service-settings`，运行中的服务会重启，停止的服务只保存文件，下次启动生效。文件位于 Compose 挂载目录，容器重建后保留；首次没有该文件时使用 Compose 命令参数，之后网页保存的参数优先。无效配置文件明确报错并保留原文件。

已有部署需使用更新后的 `ctools/dram-bw/Dockerfile` 重新构建并部署 `dram-bw` 镜像。接口检查镜像能力标签及启动入口，不支持网页参数的旧镜像会提示更新；配置保留在数据库，可在部署完成后重新保存或启动服务重试下发。下发失败、重启失败或启动后立即退出都会显示错误；运行状态仍以服务容器观测为准。

## DRAM 的授权与挂载

DRAM 与 Rootless Docker 分别设置使用者列表、节点管理 socket 和容器内采集 socket 路径。DRAM 默认不授权任何使用者；默认管理路径为 `/run/dram-bw-manager/control.sock`，容器内采集路径为 `/run/dram-bw/control.sock`。勾选使用者并保存后，只挂载到此节点已接管、已初始化且归属该使用者的容器。未创建或未启动的容器会在就绪后应用；不会为挂载而启动用户容器。

DRAM 镜像内的独立管理进程复用热挂载与身份校验逻辑，不启动或依赖 Rootless Docker 引擎。业务容器只得到采集 socket，管理接口与 `/run/dram-bw/service-settings` 均不会挂入。采样 socket 保留 `0660` 和部署时的 `DRAM_BW_GID`；容器内非 root 客户端还需匹配该组。数据表示宿主机总带宽，不是单个容器独占带宽。撤销挂载阻止后续通过该路径连接，不回收客户端已经获得的连接或共享内存。

管理 socket 所在目录为 `/run/dram-bw-manager`，持久化挂载记录保存在 `/var/lib/dram-bw-manager`。worker 服务账号需属于部署的授权组；管理 socket 权限等同宿主机管理员。采集、管理和记录目录应由 root 所有，不能由组或其他用户写入。Compose 使用 `privileged`、`pid: host`、`userns_mode: host`；无需共享 IPC namespace。普通用户日常通过网页操作，无需 sudo。

取消授权或更换容器内路径时，先撤销旧挂载；有记录的容器在撤销完成前不能转交、删除或解除接管。挂载失败保留记录和错误，可用“重试应用挂载”恢复。停止服务会撤销网页管理的挂载，启动后按保存的选择恢复。服务正常退出还会卸载命令行关联，保留其关联记录供下次启动恢复。容器启动、恢复以及采集 socket 重建由 Docker 事件和文件事件驱动，不使用定时健康检查。只支持本机 rootful Docker、无 userns-remap 的目标容器及 Linux 5.6+ x86_64/aarch64。

两个服务独立显示挂载状态和错误；同一容器内的目标路径不能被两个服务共用。命令行管理 DRAM 关联时，设置 `ROOTLESS_CONTROL_SOCKET=/run/dram-bw-manager/control.sock`，使用 `rootless-docker add/remove/bindings`；`add/remove` 需显式指定 `--socket-path /run/dram-bw/control.sock`。DRAM 管理接口只接受挂载、撤销和查询，不接受引擎或代理操作。

总控与 worker 的业务协议同步更新，需使用匹配的构建。已有部署需要更新主程序及 Rootless 管理镜像，并重建 DRAM 服务镜像；发布更新器会自动打包采集与管理二进制，补齐所需 namespace 和管理目录挂载，保留已有部署参数、采集配置与数据。手动部署时沿用原来的 GID、环境文件和 Compose override，从仓库根目录执行：

```bash
DRAM_BW_GID=<原授权组数字GID> docker compose -f deploy/services.yaml up -d --build --no-deps dram-bw
```

## Rootless Docker 的挂载范围

每个节点独立设置管理 socket 路径、容器内业务 socket 路径和使用者列表。默认管理路径为 `/run/rootless-docker/control.sock`，容器内路径为 `/var/run/docker.sock`，可一键恢复默认值；路径须包含 socket 文件名。默认不选择任何使用者。worker 服务账号需能访问本机 rootful Docker 与 Rootless 管理 socket；按 [Rootless Docker 部署说明](rootless-docker.md) 设置授权组。普通业务容器只得到 rootless dockerd 的业务 socket，不得到管理 socket。

勾选使用者并保存后，挂载到此节点已接管且已初始化的对应容器。尚未拥有容器的使用者也可预先选择；平台创建、领养、初始化容器或修改归属后应用选择。已停止或暂停的容器保存关联，等待 Docker 启动或恢复事件后挂载，不为挂载而启动用户容器。

所选使用者共用同一个 rootless Docker 引擎，可管理其中全部容器、镜像和卷；此选择是访问授权，不提供使用者之间的引擎隔离。取消勾选或修改目标路径时先撤销旧挂载，再保存和应用新配置。有挂载记录的容器必须先取消挂载，才能转给其他使用者、回收资源、删除或解除接管。失败记录也受此保护，避免失败的操作留下未受追踪的访问权限。

网页显示受管理挂载和命令行关联。相同 endpoint、容器 ID、目标路径的现有关联被选中后纳入网页管理，其余命令行关联通过 `rootless-docker remove` 管理。挂载状态通过实时检查获取；服务不可达时显示未知状态。

## 启停与恢复

- **停止**：先撤销网页管理的挂载并移除这些 daemon 关联，然后按配置的等待时间停止服务容器。Rootless 默认等待 120 秒，保留使用者选择及 rootless Docker 数据。撤销失败则不继续停止，保留可重试记录。命令行关联由 daemon 退出时卸载，原关联仍保留。
- **启动 / 重启**：应用已保存的自动重启策略；Rootless 等待管理接口及 dockerd 就绪，再为所选使用者恢复挂载。重启前先撤销网页管理的旧挂载，避免旧 socket inode 残留。
- **worker 重启**：读取配置和操作记录，只为已运行的 Rootless 服务恢复挂载，不自动启动被停止的服务。
- **目标容器或 dockerd 重启**：复用 Rootless 已有 Docker 事件连接恢复。关联绑定完整容器 ID，同名重建不沿用旧 ID；由平台创建并分配的新容器按当前使用者选择建立新关联。
- **失败或请求中断**：可能已有部分操作完成。刷新核对状态后，使用“重试应用挂载”重试；原记录和错误不会因失败自动清除。

容器创建或初始化成功后，即使挂载失败也会正常返回密码，并单独提示重试挂载；启动、重启和分配只同步本次容器。归属保存后的挂载失败也作为提示返回。

网页服务操作与总控转发支持较长的停止和恢复时间。如果部署了其他反向代理，也需允许该操作的请求时间。

## 更新与数据库

主程序与 Rootless 管理容器必须使用匹配的构建。正常发布更新会安装程序、同步已部署的节点服务容器并原地升级数据库；主程序已是目标版本时，也会继续检查容器。管理接口提供 `bindings` 实时检查命令。权限、同版本容器同步及故障恢复见[更新器说明](updater.md)。

若出现 `Rootless 管理请求失败（400）：未知命令：bindings`，说明当前管理 socket 连接的 daemon 不支持挂载检查命令，并非残留配置导致。只更新主程序或宿主机的 client 二进制不会更新运行中的管理容器；需要从与主程序匹配的源码重新构建镜像并重建该服务容器。保留原部署的授权 GID、环境文件和 Compose override，在新源码目录执行：

```bash
ROOTLESS_DOCKER_GID=<原授权组数字GID> docker compose -f deploy/services.yaml up -d --build --no-deps rootless-docker
```

使用额外环境文件或 override 的部署需在命令中沿用对应的 `--env-file`、`-f` 参数，并确认 override 未指向旧源码的构建目录。更新会短暂停止 rootless 引擎，已有账户、Docker 数据及关联记录保留；不需要删除 `bindings.json` 或清空 worker 数据库。更新后使用 `rootless-docker bindings`（自定义管理路径时设置 `ROOTLESS_CONTROL_SOCKET=<路径>`）验证，再回到网页刷新并重试应用挂载。若仍报告未知命令，检查所配置的管理 socket 是否连接到了其他旧 daemon。

DRAM 与 Rootless 挂载分别记录服务归属；“修改容器归属、删除或解除接管前，请先取消服务挂载”适用于两种服务。

服务配置和网页挂载操作记录保存在 worker 数据库。初始化和 updater 使用同一 schema；control、registry 同步推进数据库版本但不创建 worker 专用表。正常发布更新自动迁移数据库。仅在手动安装程序后单独维护数据库时，停止使用该数据目录的服务，执行 `alpha-updater --database-only --data-dir <原目录>`。目标版本查看 `alpha-updater --help`，支持窗口查看生成的 `internal/platform/upgrade_history.json`；完整操作见 [更新器说明](updater.md)。升级保留已有容器记录、使用者、密钥、服务重启策略和挂载授权；已有 DRAM 服务配置原地补齐授权路径，保留采集参数；原有挂载记录归入 Rootless Docker，DRAM 不会自动获得任何使用者授权。

## 授权挂载验证

后端和升级器回归：`go test ./internal/containers ./internal/rootless ./internal/updater`。浏览器回归：`python3 tests/test_node_services_browser.py`（需要 Playwright）。

真实 DRAM 挂载测试使用独立容器和临时目录，验证默认不授权、样本读取、重复挂载、停止容器、容器与服务重启恢复，以及撤销。需已有 Docker 权限和本地 `alpine:latest` 镜像，从仓库根目录执行：

```bash
mkdir -p /tmp/alpha-dram-access
CGO_ENABLED=0 go build -o /tmp/alpha-dram-access/rootless-docker ./cmd/rootless-docker
make -C ctools/dram-bw BUILD=/tmp/alpha-dram-access LDFLAGS=-static \
  /tmp/alpha-dram-access/dram-bwd /tmp/alpha-dram-access/dram-bw-consume
DRAM_TEST_BIN_DIR=/tmp/alpha-dram-access python3 tests/test_dram_mounts_compose.py
ALPHA_UPDATER_DOCKER_TEST=1 go test ./internal/updater -run '^TestDockerContainerUpdate$' -v
```
