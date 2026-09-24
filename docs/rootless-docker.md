# rootless-docker

在 Linux 宿主机创建专用用户 `docker-rootless`，运行独立的 rootless dockerd，并把它的 Unix socket 热挂载到已有容器。作为独立的 Go 命令集成到 project-alpha，不接入 Web，也不使用平台数据库。运行时不依赖 Python。

在项目根目录构建：

```bash
go build -o bin/rootless-docker ./cmd/rootless-docker
./bin/rootless-docker --help
```

配置子进程先启动，再将全部线程的 UID/GID 与附加组切换为专用用户，完成降权后才读写配置；二进制无需向专用用户开放执行权限。热挂载在单独的挂载工作进程中执行。部署时可将二进制安装到 `/usr/local/bin/rootless-docker`。

## 使用

需要宿主机运行 systemd，已安装 Docker Engine、`docker-ce-rootless-extras`、`uidmap`、`slirp4netns`、`iptables` 和用户 D-Bus。`add` 需要 x86_64/aarch64 Linux 5.6+。Ubuntu/Debian 已配置 Docker 官方软件源时：

```bash
sudo apt-get install uidmap dbus-user-session slirp4netns iptables docker-ce-rootless-extras

sudo ./bin/rootless-docker init
sudo ./bin/rootless-docker add <容器名或ID>
```

`init` 在用户不存在时创建无登录权限的用户，使用现有用户时保留其家目录与 UID，并检查该用户不属于 `root`/`docker` 组；检查或补充分配不与现有范围冲突的 65536 个 subordinate UID/GID。随后创建用户级 `docker-rootless.service`，启用 linger，启动服务并验证实际 daemon 的 rootless 标记和数据目录。可重复执行；不会自动重启已经运行的 daemon。

`add` 的目标容器必须正在运行，且属于本机 rootful Docker。目前不支持目标容器启用 `userns-remap`。默认挂到 `/var/run/docker.sock`；如果原来已有 socket，会在容器的挂载命名空间内覆盖它，不删除或改写原 socket。

```bash
# 容器中需已有 Docker CLI；用 root 用户验证 socket 访问权限
sudo docker --host unix:///var/run/docker.sock exec -u 0 <容器名> \
  docker --host unix:///var/run/docker.sock info

# 自定义容器内路径，或指定目标容器所在的本地 daemon
sudo ./bin/rootless-docker add <容器名> --socket-path /run/rootless-docker.sock
sudo ./bin/rootless-docker add <容器名> --host unix:///另一个本地/docker.sock

# 直接管理独立 daemon，CLI 参数原样传递
sudo ./bin/rootless-docker docker ps -a
sudo ./bin/rootless-docker docker run --rm hello-world
sudo ./bin/rootless-docker status
sudo ./bin/rootless-docker logs
sudo ./bin/rootless-docker stop
sudo ./bin/rootless-docker start
sudo ./bin/rootless-docker restart
```

## Registry 拉取代理

配置 rootless dockerd 的代理（Docker Engine 23.0+）。将下面的 `192.168.1.10:7890` 换成 rootless 网络中可访问的 HTTP 代理地址：

```bash
# 已初始化的服务：保存代理配置，并自动重启 rootless daemon
sudo ./bin/rootless-docker proxy \
  --http-proxy http://192.168.1.10:7890 \
  --https-proxy http://192.168.1.10:7890 \
  --no-proxy localhost,127.0.0.1,.internal

# 也可以在首次初始化时配置，参数与 proxy 相同
sudo ./bin/rootless-docker init \
  --http-proxy http://192.168.1.10:7890 \
  --https-proxy http://192.168.1.10:7890

sudo ./bin/rootless-docker proxy                         # 查看已保存配置，隐藏认证信息
sudo ./bin/rootless-docker proxy --http-proxy ''           # 仅清除 HTTP 代理
sudo ./bin/rootless-docker proxy --clear                  # 清除全部代理并重启

# 验证 rootless daemon 的 registry 拉取
sudo ./bin/rootless-docker docker pull hello-world
sudo ./bin/rootless-docker test check                         # 自动重新挂载并验证容器内 CLI
```

代理保存在用户家目录下的 `.docker-rootless/config/daemon.json` 的 `proxies` 字段，作用于 daemon 的镜像拉取/推送。`HTTPS` registry 通常也使用 `http://` 的代理 URL，通过 CONNECT 隧道连接。未指定的字段保持原值；不带代理参数重新执行 `init` 会保留已有代理。更新前会调用 `dockerd --validate` 校验候选配置，校验失败保留原配置。查看命令显示的是已保存配置，不表示代理连通性检查。

设置、清除代理或带代理参数执行 `init` 会重启 rootless daemon；已有容器需重新执行 `add`，或通过 `rootless-docker test exec/check` 自动重新挂载。启动脚本清除继承的代理环境变量，以此配置文件为准。

本工具使用独立 RootlessKit 网络，并默认禁止访问宿主机回环地址。若代理监听在宿主机的 **`127.0.0.1:13099`**，执行：

```bash
sudo ./bin/rootless-docker proxy --allow-host-loopback \
  --http-proxy http://10.0.2.2:13099 \
  --https-proxy http://10.0.2.2:13099 \
  --no-proxy localhost,127.0.0.1,.internal

sudo ./bin/rootless-docker docker pull hello-world
# 如需在测试容器内验证，此命令会重新挂载 socket
sudo ./bin/rootless-docker test check
```

在本工具使用的 slirp4netns 默认网络里，`10.0.2.2` 用于访问宿主机的 IPv4 回环地址；rootless 中的 `localhost` / `127.0.0.1` 指向 rootless 网络自身。代理需监听宿主机 IPv4 `127.0.0.1`，仅监听 IPv6 `::1` 不适用。

`--allow-host-loopback` 会放开 rootless 网络对宿主机回环服务的访问，**不限于 13099 端口**。设置保存在 `.docker-rootless/config/rootlesskit.json`，以后执行 `init` 或修改代理时会保留；`proxy` 可查看该设置。可用 `sudo ./bin/rootless-docker proxy --disable-host-loopback` 恢复禁止访问。`--clear` 只清除代理地址；若同时恢复默认网络限制，使用 `sudo ./bin/rootless-docker proxy --clear --disable-host-loopback`。`init` 也接受这两个网络选项。

此配置只作用于独立 rootless daemon；`test` 子命令创建 CLI 容器时，`docker:28-cli` 镜像仍由 root dockerd 拉取。

参考：[Docker daemon 代理配置](https://docs.docker.com/engine/daemon/proxy/)、[slirp4netns 宿主机回环访问](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md#filtering-connections)。

## 交互测试容器

`rootless-docker test` 使用含 Docker CLI 的 [Docker 官方镜像](https://github.com/docker-library/docs/blob/master/docker/README.md)，在 root dockerd 下创建普通测试容器，再调用本项目的 `add` 热挂载 rootless socket。默认镜像为 `docker:28-cli`。

```bash
# 首次配置独立 daemon；已初始化可跳过
sudo ./bin/rootless-docker init

sudo ./bin/rootless-docker test up       # 创建并挂载，验证 daemon ID
sudo ./bin/rootless-docker test exec     # 进入 sh；也可直接执行此命令，自动创建并挂载

# 在容器终端中执行
docker version
docker ps
docker run --rm hello-world
exit

# 或自动执行 Docker CLI / hello-world 检查
sudo ./bin/rootless-docker test check
sudo ./bin/rootless-docker test cleanup
```

默认测试容器名为 `rootless-cli-test`。各命令都可追加自定义容器名，例如 `sudo ./bin/rootless-docker test exec my-cli-test`，随后用 `sudo ./bin/rootless-docker test cleanup my-cli-test` 清理。自定义镜像可用 `sudo ROOTLESS_TEST_IMAGE=docker:28-cli ./bin/rootless-docker test up`；镜像需包含 `/bin/sh`、`tail` 和 Docker CLI。

`up`、`exec`、`check` 会重新挂载 socket，因此能恢复容器或 daemon 重启后失效的挂载。同名的非本工具容器会被拒绝操作。挂载或验证失败时保留带标记的测试容器，便于重试或执行 `cleanup`。

`cleanup` 仅删除带有本工具标记的 CLI 测试容器及其匿名卷，不删除镜像缓存，不停止 rootless daemon。`check` 创建的 `hello-world` 容器通过 `--rm` 自动删除；在交互终端中手工创建的其他容器、卷等资源需要自行清理。首次拉取镜像需要能访问镜像仓库。

## 存放位置与隔离

默认家目录是 `/home/docker-rootless`；若用户已存在，使用 passwd 记录的家目录。以下路径均相对于该家目录：

| 路径 | 内容 |
| --- | --- |
| `.docker-rootless/data/` | 镜像、容器、卷及由该 dockerd 自行启动的 containerd 数据 |
| `.docker-rootless/config/daemon.json` | 独立 daemon 配置 |
| `.docker-rootless/config/rootlesskit.json` | 宿主机回环访问设置 |
| `.docker-rootless/run/` | socket、PID、exec-root、RootlessKit 状态 |
| `.docker-rootless/tmp/` | dockerd 临时文件 |
| `.docker-rootless/log/dockerd.log` | daemon 标准输出和错误日志 |
| `.docker-rootless/client/` | 此用户的 Docker CLI 配置 |
| `.docker-rootless/cache/` | 缓存 |
| `.docker-rootless/launch.sh` | 服务启动脚本 |
| `.config/systemd/user/docker-rootless.service` | 用户级 systemd 服务 |

工具不修改系统 Docker 的配置、数据目录、socket、服务或 CLI context，也不把此用户加入 `docker` 组。RootlessKit 提供独立的用户、挂载和网络命名空间；dockerd 自行启动 containerd，不接入系统 containerd。只通过宿主机 Docker 查询目标容器，然后在目标容器内增加挂载。

创建用户必需的 `/etc/passwd`、`/etc/shadow`、`/etc/group`、`/etc/subuid`、`/etc/subgid` 以及 systemd linger、用户 D-Bus、cgroup 属于系统账户/服务管理设施，不能搬进家目录。Docker 持久化数据和工具管理文件放在家目录；systemd 本身仍可能记录服务生命周期日志。不要把家目录放在 NFS 上。

## 热挂载的边界

- 不重建、不重启目标容器，也不要求容器内有 shell 或 `mount`。挂载不会写入 Docker 的容器创建配置，所以 `docker inspect` 的 `Mounts` 不会列出它。
- **目标容器重启、重建或 rootless dockerd 重启后，需要重新执行 `add`。** 相同 socket 重复执行不会叠加挂载；daemon 重启后会先解除失效的旧 socket 挂载，再挂载新 socket。
- 如果需要挂载随容器启动自动恢复，应在创建容器时使用 Docker/Compose 声明挂载；本工具的 `add` 专用于已有运行容器。
- socket 权限保持 `0660`；默认允许目标容器的 root 用户访问。非 root 应用需具备对应 socket GID 的组权限；工具不会将 socket 改成 `0666`，也不会修改运行中进程的附加组。
- 既有进程若已连上旧 socket，需主动断开重连。工具不会修改容器现有进程的 `DOCKER_HOST` 或 Docker context；客户端应显式使用上面示例中的 `--host`。
- 获得 socket 的容器可以管理这个 rootless daemon 下的所有容器和卷。它创建的是独立 daemon 下的容器，bind mount 的源路径按宿主机文件系统解释，受 `docker-rootless` 用户权限限制。
- `add` 会拒绝共享传播的目标挂载，避免新增挂载传播回宿主机。容器根文件系统只读且目标路径不存在时，需先准备目标文件或选择可写路径。
- 普通 daemon 日志集中写入 `dockerd.log`；长期使用可自行配置轮转。容器日志默认使用 Docker 的 `local` 驱动。

修改 `.docker-rootless/config/daemon.json` 后执行 `restart`。可添加镜像源、DNS 等设置；保持 `data-root`、`exec-root`、`pidfile`、`hosts`、`rootless`、`group` 的隔离配置不变，不指定外部 `containerd`。

## 验证

以下命令在项目根目录执行。普通回归测试无需 root、Docker daemon 或 systemd：

```bash
go test ./internal/rootless ./cmd/rootless-docker
```

使用本机已安装的 dockerd、systemd 校验临时生成的配置（不启动服务）：

```bash
ROOTLESS_INTEGRATION=1 go test ./internal/rootless -run '^TestInstalledConfigValidators$' -v
```

真实网络测试只使用临时回环监听端口，需要当前用户可运行 RootlessKit：

```bash
ROOTLESS_NETWORK_INTEGRATION=1 go test ./internal/rootless -run '^TestRealHostLoopback$' -v
```

真实降权测试通过 RootlessKit 的 subordinate UID/GID 映射运行，覆盖 `0700` 二进制、配置文件属主及多个 Go 线程的权限清除。只使用临时目录，不创建宿主机用户或服务；不要作为宿主机 root 运行：

```bash
go test -c -o /tmp/project-alpha-rootless.test ./internal/rootless
ROOTLESS_WORKER_INTEGRATION=1 rootlesskit \
  /tmp/project-alpha-rootless.test -test.run '^TestWorkerPrivilegeIntegration$' -test.v
```

也可使用 `CGO_ENABLED=0 go test -c ...` 构建测试程序，验证不依赖 libc 的降权路径。

真实 socket 挂载测试只使用临时目录及隔离的 user/mount namespace，不操作 Docker 或创建系统用户；不要直接作为宿主机 root 运行：

```bash
go test -c -o /tmp/project-alpha-rootless.test ./internal/rootless
ROOTLESS_MOUNT_INTEGRATION=1 unshare --user --map-root-user --mount --fork \
  /tmp/project-alpha-rootless.test -test.run '^TestRealSocketMounts$' -test.v
```

挂载测试覆盖已有 socket、缺少目标文件、重复挂载、源 socket 重建，以及共享传播、非空文件、源符号链接和错误属主的拒绝行为。真实 Docker 全流程通过上文 `test up/exec/check/cleanup` 手动执行。

实现依据：[Docker rootless 前提条件](https://docs.docker.com/engine/security/rootless/)、[用户级服务及目录说明](https://docs.docker.com/engine/security/rootless/tips/)、[dockerd 参数](https://docs.docker.com/reference/cli/dockerd/)、[内置 containerd](https://docs.docker.com/engine/daemon/embedded-containerd/)。
