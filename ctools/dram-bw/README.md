# dram-bw

在 Linux 宿主机上采集 DRAM 带宽，供本机或容器内的程序读取。源码位于 project-alpha 的 `ctools/dram-bw/`，独立构建和安装，在 GitHub Release 中以 `ctools-dram-bw_*.tar.gz` 单独提供。

- **服务端是 `dram-bwd`**：读取硬件计数器、计算带宽并分发样本。
- **命令行客户端是 `dram-bw-consume`**：连接服务端，读取指定条数，输出 CSV 后退出。
- **自己的应用使用 `libdram_bw.a` 或 `libdram_bw.so`**：两者提供相同的 C API，分别用于静态链接和动态链接。

```text
硬件 PMU → dram-bwd → 每客户端独立的共享内存队列 → dram-bw-consume → CSV
                                                → 你的应用 + libdram_bw
```

客户端通过 Unix socket 控制订阅，通过共享内存读取样本，不需要 PMU 权限。
结果是计数器覆盖范围内的宿主机内存流量，不能用来归属单个容器的独占带宽。

## 导航

- [编译与产物](#编译与产物)：每个文件是什么、由哪个 make 目标生成。
- [先跑通模拟示例](#先跑通模拟示例)：无需硬件权限的双终端示例。
- [命令与客户端 API](docs/usage.md)：参数、前置条件、输出和示例。
- [采集真实硬件数据](#采集真实硬件数据)：驱动准备、`make server` 和权限。
- [辅助脚本](#辅助脚本)：服务启动、perf 对照、离线分析。
- [安装与容器接入](#安装与容器接入)：安装位置、systemd、Docker。
- [常见问题](#常见问题)。

测量公式、完整状态位和 IPC 细节见 [技术参考](docs/reference.md)；测试方法、覆盖范围和迁入前的硬件对照记录见 [验证说明](docs/validation.md)。

## 编译与产物

依赖：64 位 Linux 5.1+、Linux 开发头文件、glibc、GCC/Clang、make；测试和辅助脚本还需要 Python 3。原子操作须原生 lock-free，内核须支持 `F_SEAL_FUTURE_WRITE`。
从仓库根目录进入工具目录；本文后续命令均在 `ctools/dram-bw/` 中执行。

```bash
cd ctools/dram-bw
make -j
make help
```

默认输出目录为 `build/`，可通过 `make BUILD=其他目录` 更改。

| 产物 | 类型与用途 | 构建命令 | 需要事先启动 daemon？ |
|---|---|---|---|
| `build/dram-bwd` | 服务端，采集并分发带宽 | `make` | 它本身就是 daemon |
| `build/dram-bw-consume` | 命令行客户端，打印 CSV | `make` | 是 |
| `build/libdram_bw.a` | 客户端静态库，链接进应用 | `make` | 应用连接时需要 |
| `build/libdram_bw.so` | 客户端动态库，运行时加载 | `make` | 应用连接时需要 |
| `build/test-core` | 核心单元测试 | `make build/test-core` | 否 |
| `build/test-client` | 客户端 API 测试 | `make build/test-client` | 是，指定配置的 mock daemon |
| `build/bench-ring` | 共享内存传输基准 | `make build/bench-ring` | 否，自行创建生产者和消费者 |
| `build/*.o` | 编译中间文件，不能直接运行 | 按依赖自动生成 | 不适用 |

`client.o`、`ipc.o` 组成客户端库；`daemon.o`、`backend.o`、`ring.o`、`ipc.o` 链接成服务端。
`dram-bw-consume` 链接的是静态客户端库，运行时无需项目的 `.so`，仍依赖系统运行库。

| make 命令 | 行为 |
|---|---|
| `make` / `make all` | 只构建两个程序和两份库，不启动服务 |
| `make test` | 构建并运行单元与集成测试；自动启动和清理 mock daemon |
| `make bench` | 构建并执行传输基准 |
| `make release` | 构建并打包 daemon、server 脚本、客户端、两份库和头文件 |
| `sudo make server` | 构建后通过 Python 包装脚本前台启动服务 |
| `sudo make install` | 安装程序、库、头文件；不安装或启动 systemd 单元 |
| `make clean` | 删除当前 `BUILD` 目录，保留测量结果 |
| `make help` | 显示构建目标和变量，不构建、不启动程序 |

所有五个可执行程序均支持 `-h` / `--help`；帮助不会连接 socket、打开 PMU 或执行测试。
帮助写到 stdout，退出码为 0；参数错误写到 stderr，退出码为 2。

### 打包 release

```bash
make -j release
# 从仓库根目录也可运行：make -C ctools/dram-bw -j release
```

默认输出 `build/release/dram-bw-linux-<架构>.tar.gz`，架构取本机 `uname -m`。可通过 `RELEASE_DIR` 修改输出目录，通过 `RELEASE_NAME` 修改包名（不含 `.tar.gz`）；指定 `BUILD` 后默认输出目录随之改变。交叉编译时应显式设置目标架构对应的 `RELEASE_NAME`。

Actions 会按 amd64、arm64 分别调用此目标，生成 `ctools-dram-bw_<版本>_linux_<架构>.tar.gz`，与主程序包一起上传并纳入 `SHA256SUMS`。普通分支构建的包可在 Actions Artifacts 下载，`v*` tag 构建的包也作为 Release 附件发布。

压缩包包含一个同名目录，内有 `bin/dram-bwd`、`bin/dram-bw-consume`、`scripts/server.py`、`lib/libdram_bw.a`、`lib/libdram_bw.so`、`include/dram_bw.h`，以及 systemd 单元、默认配置和包内 README。每次从临时目录打包，成功后替换同名压缩包；不包含测试程序、对象文件或实验数据。

解压后的运行、库链接和安装命令见 [release 包说明](docs/release.md)。包内程序使用构建环境的架构及系统运行库；目标机器不需要编译器，运行 server 脚本另需 Python 3。

## 先跑通模拟示例

在两个终端中进入 `ctools/dram-bw/`，先完成 `make -j`。

终端一：启动模拟服务，保持此终端运行。

```bash
mkdir -p /tmp/dram-bw-demo
./build/dram-bwd --backend mock \
  --socket /tmp/dram-bw-demo/control.sock \
  --interval-us 1000 --peak-gbps 3
```

终端二：读取 10 条样本后退出。

```bash
./build/dram-bw-consume /tmp/dram-bw-demo/control.sock 10
```

CSV 中应看到读 `1.000000`、写 `0.500000`、总带宽 `1.500000` GB/s，利用率 `0.500000`。
这是显式生成的模拟数据，样本带 `MOCK` 标志。完成后在终端一按 Ctrl-C 停止服务。

## 采集真实硬件数据

Rome 宿主机需要 `amd_uncore` 驱动，以及 `/sys/bus/event_source/devices/amd_df`。
在 Ubuntu 上若缺少模块，可安装与运行内核匹配的 `linux-modules-extra-$(uname -r)` 包。

```bash
sudo modprobe amd_uncore
cat /sys/bus/event_source/devices/amd_df/cpumask
make -j
sudo make server
```

看到 `READY` 后，在另一个终端以原普通用户运行：

```bash
./build/dram-bw-consume /run/dram-bw/control.sock 10
```

`make server` 调用 `scripts/server.py`，再启动同一个 `dram-bwd`，没有另一个 server 二进制。
它使用 Rome 后端、1 秒采样等待间隔，开启诊断；直接运行 `dram-bwd` 的默认等待间隔为 100 ms，诊断关闭。
包装脚本通过 `SUDO_UID` / `SUDO_GID` 为调用 sudo 的普通用户配置访问权限。
目录为 `0750`，socket 为 `0660`，并用 `--allow-uid` 检查连接 UID。

| Make 变量 | 默认值与作用 |
|---|---|
| `SERVER_DIR` | `/run/dram-bw`，存放 socket、会话信息和诊断 |
| `SERVER_BACKEND` | `amd-rome`；可设 `mock` 验证启动流程 |
| `SERVER_INTERVAL_US` | `1000000`，传给 daemon 的等待间隔 |
| `SERVER_UID` / `SERVER_GID` | sudo 调用者的 UID/GID，否则当前用户；root shell 中应显式指定目标用户 |
| `SERVER_REFERENCE` | `0`；设为 `1` 同时启动 perf 对照，仅支持 Rome |

会话信息位于 `/run/dram-bw/current.json`，诊断位于独立的 `/run/dram-bw/session-*/`。
Ctrl-C 停止脚本启动的进程，保留日志。硬件访问需要 root 或内核策略允许的 `CAP_PERFMON`。
自行管理权限时，也可直接启动：

```bash
sudo install -d -m 0755 /run/dram-bw
sudo ./build/dram-bwd --backend amd-rome --interval-us 100000
```

直接启动时 socket 的组由服务进程的运行组决定，普通客户端须属于授权组。不同启动方式不要同时占用同一个 socket。

## 辅助脚本

这些 Python 文件不需要编译，都支持 `-h` / `--help`。

| 脚本 | 用途与输出 | 用法 |
|---|---|---|
| [`scripts/server.py`](scripts/server.py) | 管理 daemon，可选启动 perf；写入会话元数据和日志 | 通常使用 `sudo make server`；直接参数见 `python3 scripts/server.py --help` |
| [`scripts/compare_perf.py`](scripts/compare_perf.py) | 施加内存负载，与准备好的 perf 会话对照；保存 CSV、诊断、NUMA 分布和 `results.json` | 见下面示例 |
| [`scripts/analyze_timing.py`](scripts/analyze_timing.py) | 离线分析已保存的对照窗口；写入 `timing/analysis.json`，重跑覆盖该文件 | 见下面示例，无需运行中的服务 |

硬件对照需要双 NUMA 节点 Rome、`perf`、`taskset` 和 Python 3：

```bash
# 终端一
sudo make server SERVER_REFERENCE=1

# 终端二，READY 后以被授权的普通用户运行；输出目录须尚不存在
python3 scripts/compare_perf.py --output results/perf-comparison --solo
python3 scripts/analyze_timing.py results/perf-comparison
```

默认每段采集 30 个样本，进行三轮无额外负载、node0、node1、双节点对照；`--solo` 先增加一轮服务单独采样。
约需 9 分钟，每节点最多使用四个物理核，双节点约 4 GiB 工作集。结束时脚本会停止自己创建的负载并关闭 perf 参考计数。
可通过 `--samples`、`--repeats`、`--conditions` 选择范围，完整参数见 `--help`。更换拓扑前应核对节点和 CPU 选择。
perf 一致性与窗口对齐的限制见 [对照细节](docs/reference.md#perf-对照细节)。

## 安装与容器接入

### 安装和 systemd

`make install` 默认安装到 `/usr/local`：`bin/` 下是两个程序，`lib/` 下是两份库，`include/` 下是 `dram_bw.h`。
可用 `PREFIX` 改安装前缀，`DESTDIR` 做打包暂存；测试程序、脚本、配置和服务单元不会自动安装。

```bash
sudo make install
sudo ldconfig
sudo groupadd --system dram-bw  # 已有该组时跳过
sudo useradd --system --gid dram-bw --no-create-home --shell /usr/sbin/nologin dram-bw  # 已有账号时跳过
sudo install -m 0644 systemd/dram-bwd.service /etc/systemd/system/
sudo install -m 0644 examples/dram-bw.default /etc/default/dram-bw
# 按硬件和采样需求编辑 /etc/default/dram-bw
sudo systemctl daemon-reload
sudo systemctl enable --now dram-bwd
sudo journalctl -u dram-bwd -f
```

单元使用 `/usr/local/bin/dram-bwd`；修改 `PREFIX` 时也要修改 `ExecStart`。
常驻服务以 `dram-bw` 用户和组运行，使用 `CAP_PERFMON` 访问 PMU，内存上限 384 MiB。普通客户端须取得该组权限或另行授权。
启用服务前先停止占用同一路径的前台实例，并准备好驱动及内核权限策略。

### 容器客户端

宿主机运行 daemon。容器只需访问 socket 所在目录，无需共享 IPC namespace 或挂载 `/dev/shm`。
下例假定宿主机已允许容器映射后的 UID/GID 访问 socket：

```bash
docker run --rm \
  --mount type=bind,src=/run/dram-bw,dst=/run/dram-bw,readonly \
  --mount type=bind,src="$(pwd)/build",dst=/opt/dram-bw,readonly \
  ubuntu:22.04 /opt/dram-bw/dram-bw-consume /run/dram-bw/control.sock 100
```

非 root 容器需匹配授权组；rootless/userns 使用映射后的宿主机 UID/GID，`--allow-uid` 也按此校验。
`make server` 默认只授权 sudo 调用者，容器映射到其他 UID 时需调整服务授权。
服务重启后客户端须重新连接；挂载目录可让新的 socket 路径可见。两端须使用兼容的 64 位本机 ABI 与运行库。

## 常见问题

| 现象 | 原因与处理 |
|---|---|
| `make` 后没有 `test-client` / `bench-ring` | 它们是额外目标，执行对应构建命令或 `make test` / `make bench` |
| `dbw_open: No such file or directory` | 先启动 daemon，核对两端 socket 路径 |
| `Permission denied` | 检查父目录和 socket 权限、运行组，以及宿主机视角的 `--allow-uid` |
| `DRAM backend unavailable` | 检查 CPU、驱动、PMU 权限或自定义事件；只试运行流程时显式选择 mock |
| `Address already in use` / 拒绝替换 socket | 确认是否已有服务；SIGKILL 可能留下路径，确认原进程退出后再清理 |
| 读写带宽或利用率是 `nan` | Rome 仅有总带宽；利用率需要参考峰值；同时检查 `INVALID` 状态位 |
| `libdram_bw.so: cannot open shared object file` | 设置动态库搜索路径，或安装库后执行 `ldconfig` |
| `dropped` 增长 | 该客户端队列满时丢新样本；提高消费速度或调整自定义客户端的容量 |

源码入口：[`src/daemon.c`](src/daemon.c) 是服务端，[`src/client.c`](src/client.c) 是客户端库，[`examples/consume.c`](examples/consume.c) 是命令行客户端。
测量结果可存放到 `results/`；构建目录、结果目录与 Python 缓存均不纳入版本管理。
