# rootless-docker

独立的 daemon/client 工具：管理 daemon 在 privileged 容器或宿主机前台运行，在宿主机以专用用户 `docker-rootless` 启动 rootless dockerd。client 通过 Unix socket 请求挂载、卸载、重启或配置代理。管理 daemon 停止时，rootless dockerd 也停止；镜像、容器和卷保留。独立工具不使用平台数据库。网页的服务配置和挂载授权保存在 worker 数据库，见[节点服务管理](node-services.md)。

## 部署

需要 Linux 宿主机运行 systemd，已安装 Docker Engine、`docker-ce-rootless-extras`、`uidmap`、`slirp4netns`、`iptables` 和用户 D-Bus。热挂载支持 x86_64/aarch64 Linux 5.6+。在已配置 Docker 官方软件源的 Ubuntu/Debian 上：

```bash
sudo apt-get install uidmap dbus-user-session slirp4netns iptables docker-ce-rootless-extras

go build -o bin/rootless-docker ./cmd/rootless-docker
./bin/rootless-docker --help

# 只启动这个服务，不启动同一文件中的 Tetragon / DRAM 服务。
ROOTLESS_DOCKER_GID=$(id -g) docker compose -f deploy/services.yaml up -d --build rootless-docker
./bin/rootless-docker status
```

[`deploy/services.yaml`](../deploy/services.yaml) 中的 `rootless-docker` 使用 `privileged`、`pid: host`、宿主机 user/cgroup namespace，绑定 `/run/rootless-docker`。静态 Go 二进制进入宿主机 mount/network namespace 和根目录后重新执行，使用宿主机的 Docker、RootlessKit、systemd 和账户信息。镜像不自动安装宿主机软件包。外层 Docker 必须是 rootful Docker。

`ROOTLESS_DOCKER_GID` 是**宿主机**授权组的数字 GID，默认为 0。控制 socket `/run/rootless-docker/control.sock` 为 `0660 root:<GID>`；获授权的 client 无需 sudo。访问此管理 socket 的权限等同宿主机管理员，不应把它暴露给普通业务容器。业务容器只获得独立 rootless dockerd 的 socket。

也可在宿主机前台运行：

```bash
sudo ./bin/rootless-docker daemon --socket-gid "$(id -g)"
# Ctrl-C / SIGTERM 停止管理服务和 dockerd。
```

每台宿主机只允许一个管理 daemon。首次启动创建无登录 shell 的 `docker-rootless` 用户，检查专用家目录、账户组及 65536 个 subordinate UID/GID，生成用户级 `docker-rootless.service`，启用用户 linger。已有用户的 UID、家目录及数据保留。每次启动会停止本工具已有的用户服务、取消其独立开机启动，再由本次管理 daemon 启动；同名非本工具服务会明确报错。

## client

```bash
./bin/rootless-docker add my-container --socket-path /run/rootless-docker.sock
./bin/rootless-docker list
./bin/rootless-docker remove my-container --socket-path /run/rootless-docker.sock

./bin/rootless-docker status
./bin/rootless-docker logs
./bin/rootless-docker restart
./bin/rootless-docker docker ps -a
./bin/rootless-docker docker run --rm hello-world
```

`add` / `remove` 默认容器内路径为 `/var/run/docker.sock`，可用 `--host unix:///其他本地/docker.sock` 选择本机 rootful Docker。客户端在其他容器中运行时，将管理 socket **所在目录**挂入，并通过 `ROOTLESS_CONTROL_SOCKET=/挂载目录/control.sock` 指定路径；需要匹配授权 GID。`docker` 子命令使用客户端本地 Docker CLI，管理服务将 Docker API 转发到 rootless dockerd，支持交互、流式输出及原 CLI 退出状态。

容器内使用业务 socket：

```bash
DOCKER_HOST=unix:///run/rootless-docker.sock docker info
```

目标容器无需重启、无需 privileged，也不需要包含 shell 或 mount 命令。运行且未暂停的目标立即挂载；停止或暂停的容器保存关联，等待启动或恢复事件后挂载。暂不支持 userns-remap。socket 保持 `0660`，默认目标容器 root 可访问；非 root 进程需要匹配 socket 的 GID。工具不会修改已有进程的附加组、`DOCKER_HOST` 或 Docker context。

## 生命周期与挂载恢复

- 管理 daemon 正常停止时，先卸载记录的 socket 挂载，再停止用户服务；保留账户、配置、关联和 Docker 数据。
- 用户服务中的非 root supervisor 持有与管理 daemon 的 Unix 控制连接。管理容器被 SIGKILL、OOM 或强制移除后，连接断开，supervisor 停止 dockerd；用户服务配置为 `Restart=no`。此机制不依赖定时健康检查。
- 强制终止无法运行管理 daemon 的挂载清理，目标容器可能暂时留下不可连接的旧 socket。下次启动根据持久化记录卸载旧挂载并恢复。
- 关联按 Docker endpoint、**完整容器 ID**和目标路径保存。目标容器重启后，通过 Docker 事件恢复挂载；事件连接重建和管理 daemon 启动时也会核对状态。容器删除后，同名新建容器需要重新 `add`。
- `restart` 或修改代理后自动更新关联的 socket。恢复错误保存在 `list` 输出中，不会静默删除关联；修复原因后重新 `add`。
- `remove` 只卸载记录的挂载并删除关联，校验宿主机启动 ID、容器启动时间、PID、mount namespace、socket inode 和 mount ID。若卸载完成后进程中断，恢复时确认原 mount ID 已从该 namespace 消失即可继续，不卸载当前路径上的其他挂载。删除关联写盘失败时保留可重试的内存记录，修复存储后可重新执行 `remove`。原文件保留；工具为挂载创建的空占位文件也保留。不会删除目标容器已有文件。
- 卸载不会终止已经建立的 socket 连接。已连接的客户端需自行断开；停止 dockerd 则会断开其 API 连接。

事件订阅由管理 daemon 直接通过 Unix socket 上的 Docker HTTP API 建立，不启动常驻 `docker events` 子进程。没有关联时不订阅，删除某个 endpoint 的最后一个关联后关闭对应连接，删除全部关联后也关闭 rootless 订阅。连接正常时阻塞等待事件，仅在断线后每秒重试；没有定时健康检查。目标订阅负责首次恢复，rootless 连接重建只处理 socket 已变化或缺失的挂载记录。无关容器事件不执行恢复，状态未变化时不重写记录或执行 fsync。

这些热挂载不写入 Docker 创建配置，`docker inspect` 的 `Mounts` 不会列出它们。新注入的 socket 挂载设为 private，不继承宿主机源挂载的共享传播；目标路径上的共享传播挂载会被拒绝。只读根文件系统需要预先准备目标文件或选择可写路径。获得业务 socket 的容器可以管理该 rootless daemon 下的全部容器和卷；bind mount 的源路径按宿主机文件系统解释，并受 `docker-rootless` 用户权限限制。

停止与启动管理容器：

```bash
docker compose -f deploy/services.yaml stop rootless-docker
docker compose -f deploy/services.yaml start rootless-docker
```

Compose 设置 `stop_grace_period: 120s`，允许服务完成正常退出。`restart: unless-stopped` 作用于管理容器；手工停止后，用户服务不会自行开机启动。

## registry 代理

Docker Engine 23.0+ 支持保存 daemon 代理配置，用于镜像拉取和推送：

```bash
./bin/rootless-docker proxy --http-proxy http://192.168.1.10:7890 \
  --https-proxy http://192.168.1.10:7890 --no-proxy localhost,127.0.0.1,.internal
./bin/rootless-docker proxy                    # 隐藏 URL 认证信息
./bin/rootless-docker proxy --http-proxy ''    # 只清除 HTTP 代理
./bin/rootless-docker proxy --clear
```

未指定字段保持不变；不带代理选项启动 daemon 会保留已有代理。保存前用 `dockerd --validate` 校验，失败保留原配置。启动脚本清除继承的代理环境变量，以 `daemon.json` 为准。HTTPS registry 通常也使用 `http://` 代理，通过 CONNECT 隧道访问。

访问只监听宿主机 IPv4 回环地址的代理：

```bash
./bin/rootless-docker proxy --allow-host-loopback \
  --http-proxy http://10.0.2.2:13099 --https-proxy http://10.0.2.2:13099
./bin/rootless-docker proxy --clear --disable-host-loopback
```

slirp4netns 中的 `10.0.2.2` 指向宿主机 IPv4 回环地址，rootless 中的 `127.0.0.1` 指向自身。该选项放开所有宿主机回环服务，不限于代理端口；仅监听 IPv6 `::1` 不适用。`daemon` 也接受这些代理和回环参数。

## 数据与权限

默认家目录 `/home/docker-rootless`；已有用户使用 passwd 记录中的家目录：

| 位置 | 用途 |
| --- | --- |
| `~/.docker-rootless/data/` | 镜像、容器、卷及独立 containerd 数据 |
| `~/.docker-rootless/config/daemon.json` | Docker 配置及代理 |
| `~/.docker-rootless/config/rootlesskit.json` | 回环访问设置 |
| `~/.docker-rootless/run/` | 业务 socket、PID、RootlessKit 和 exec-root |
| `~/.docker-rootless/log/dockerd.log` | dockerd 标准输出和错误 |
| `~/.docker-rootless/client/`、`cache/`、`tmp/` | 专用 CLI 配置、缓存和临时文件 |
| `~/.docker-rootless/launch.sh` | 降权后的启动脚本 |
| `~/.config/systemd/user/docker-rootless.service` | 受管理 daemon 生命周期约束的用户服务 |
| `/run/rootless-docker/` | root 所有的控制 socket 和锁；lease socket 仅专用用户可连接 |
| `/usr/local/libexec/rootless-docker/supervisor` | root 所有的生命周期监督程序；启动前校验专用用户可执行 |
| `/var/lib/rootless-docker/bindings.json` | root 所有、权限 0600 的关联及挂载身份记录 |

记录损坏或格式不匹配时明确报错，保留原文件，不自动重建。专用家目录不得位于 NFS。账户数据库、subuid/subgid、linger、用户 D-Bus 和 cgroup 仍使用宿主机设施。工具不修改系统 Docker 的配置、数据目录、服务或 CLI context，不把专用用户加入 root/docker 组。

rootless dockerd 自行启动 containerd，不接入系统 containerd。修改 `daemon.json` 后可执行 `restart`；保持 `data-root`、`exec-root`、`pidfile`、`hosts`、`rootless`、`group` 的隔离配置不变，不指定外部 containerd。日志集中写入 `dockerd.log`，可自行配置轮转。

## 验证

```bash
go test -race ./internal/rootless ./cmd/rootless-docker
ROOTLESS_INTEGRATION=1 go test ./internal/rootless -run '^TestInstalledConfigValidators$' -v

go test -c -o /tmp/project-alpha-rootless.test ./internal/rootless
ROOTLESS_MOUNT_INTEGRATION=1 unshare --user --map-root-user --mount --fork \
  /tmp/project-alpha-rootless.test -test.run '^TestRealSocketMounts$' -test.v
```

挂载测试在隔离 user/mount namespace 中验证实际 socket 通信、重复挂载、卸载恢复原文件、失效 socket 清理、共享传播拒绝及错误身份拒绝，不修改宿主机账户或服务。协议测试使用临时 Unix socket，覆盖 HTTP 事件订阅、断线恢复、删除最后一个关联后关闭连接、重复操作不写盘及写入失败后重试；生命周期测试覆盖控制连接断开、终止信号、子进程失败和超时终止。

两个临时容器之间的 namespace 切换、保持 cgroup 归属、重新执行，以及真实 UID 65534 supervisor 与 root 控制 socket 的断连退出，可用以下命令验证。它不使用宿主机 PID namespace、根目录或服务，需要本地已有 `alpine:latest`：

```bash
CGO_ENABLED=0 go test -c -o /tmp/project-alpha-rootless-static.test ./internal/rootless
ROOTLESS_TEST_BINARY=/tmp/project-alpha-rootless-static.test python3 tests/test_rootless_isolated.py
```

完整 Compose/宿主机联动测试为显式选择执行的 `tests/test_rootless_compose.py`。它要求宿主机已有空闲的本工具 rootless 服务、构建好的镜像，以及由 `ROOTLESS_ORIGINAL_BINARY` 和 `ROOTLESS_CLIENT_BINARY` 指定的原 CLI 与新 CLI；设置 `ROOTLESS_HOST_INTEGRATION=1` 执行。测试会备份配置及原有 socket 挂载，实际启停服务并发送 SIGKILL，最后恢复原服务、挂载并核对已有容器、镜像、卷及 daemon ID；运行中的 rootless 工作负载和已有管理 daemon 会被拒绝。

已在 Linux 5.15、Docker 28.0.1 的宿主机通过完整联动测试，覆盖 `/run` 为 noexec 和宿主机源挂载具有共享传播的环境，以及目标容器重启、client 或外部 systemctl 重启 dockerd、正常停止和 SIGKILL 后恢复挂载，并确认事件订阅不启动 Docker CLI 子进程。测试结束后原服务、配置及已有业务 socket 挂载已恢复，原 Docker 数据清单核对一致。

运行管理服务后，可用官方 `docker:28-cli` 镜像创建普通 CLI 测试容器：

```bash
./bin/rootless-docker test up
./bin/rootless-docker test check
./bin/rootless-docker test exec
./bin/rootless-docker test cleanup
```

`test exec` 使用客户端的宿主机 Docker 权限打开交互终端；其他测试操作通过管理 API。`cleanup` 只删除带本工具标签的测试容器及其匿名卷，不清理镜像和 rootless 数据。可追加自定义测试容器名。首次拉取镜像需要仓库网络访问。

参考：[Docker rootless 前提](https://docs.docker.com/engine/security/rootless/)、[用户服务与资源限制](https://docs.docker.com/engine/security/rootless/tips/)、[daemon 代理](https://docs.docker.com/engine/daemon/proxy/)、[slirp4netns 回环访问](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md#filtering-connections)。
