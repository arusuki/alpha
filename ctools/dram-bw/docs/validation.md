# 验证与复现

## 自动验证

在仓库根目录执行 `make -C ctools/dram-bw -j test`，或进入 `ctools/dram-bw/` 后运行下列命令。测试使用临时目录和显式 mock 后端，无需 root、PMU 权限或额外 Python 包。

验证包括：

- GCC 默认构建与 `make test`。
- Clang 构建与同一套测试。
- UBSan 构建与同一套测试，启用 `-fno-sanitize-recover=all`。
- Clang 静态分析全部 `src/*.c`，没有诊断。
- `make bench`：两个进程，复制/零拷贝各测试 1、64、256 的最大批量，共传输并校验 1200 万条记录。

测试需要允许本地 Unix socket 的创建、连接及通知发送；限制这些操作的沙箱不能运行完整测试。

复现命令：

```bash
make -j
make test
make bench
make BUILD=/tmp/dbw-clang CC=clang -j4 test
make BUILD=/tmp/dbw-ubsan \
  CFLAGS='-O1 -g -std=c11 -Wall -Wextra -Wpedantic -Werror -fPIC -fsanitize=undefined -fno-sanitize-recover=all' \
  LDFLAGS='-fsanitize=undefined' -j4 test
# 可选：静态分析
for f in src/*.c; do
  clang --analyze -std=c11 -Iinclude -Isrc -Xanalyzer -analyzer-output=text "$f"
done
```

C 源码格式由 [`ctools/dram-bw/.clang-format`](../.clang-format) 定义，可用 `clang-format -i src/*.[ch] include/*.h examples/*.c tests/*.c` 整理。`make test` 也运行 Python 脚本回归，检查范围格式的 CPU 列表在在线汇总与离线分析中的一致性。

## 测试与基准入口

### `test-core`

核心单元测试，覆盖 ring、计数换算、配置校验、诊断和 IPC。无需 daemon 或硬件权限，使用本地 socket 和临时文件。

```bash
make build/test-core
./build/test-core --help
./build/test-core
```

不接受位置参数。通过时输出 `core: ... passed`，失败时断言终止。

### `test-client`

客户端 API 测试，覆盖启停、零拷贝、批量读取及慢消费者隔离。它会断言 mock 数值，不能用于读取真实硬件。
通常直接用 `make test`，由集成测试脚本自动准备 daemon。

```bash
make test
./build/test-client --help
```

手动运行时，先执行 `make all build/test-client`，再在两个终端分别运行：

```bash
# 终端一：这些 mock 参数是测试前提
mkdir -p /tmp/dram-bw-test
./build/dram-bwd --backend mock --interval-us 1000 --peak-gbps 3 \
  --socket /tmp/dram-bw-test/control.sock

# 终端二
./build/test-client /tmp/dram-bw-test/control.sock
```

语法：`test-client [选项] SOCKET`。通过时输出 `client: ... passed`，失败时断言终止。手动启动的服务完成后用 Ctrl-C 关闭。
集成测试脚本也可单独运行：`python3 tests/integration.py build`，帮助为 `python3 tests/integration.py --help`；需先构建测试所需产物。

### `bench-ring`

衡量共享内存记录传输开销，自行创建生产者和消费者两个进程，无需另开 daemon 或使用 PMU。
复制和零拷贝两种模式各测试 1、64、256 的最大批量，每种情况传输并校验 200 万条合成记录。

```bash
make build/bench-ring
./build/bench-ring --help
./build/bench-ring > ring.csv
# 或：make bench，构建后直接运行
```

不接受位置参数。CSV 列是 `mode,batch,records,ns_per_record,million_records_per_sec`，分别表示模式、批量、记录数、每条记录纳秒数和每秒百万条记录数。
这是忙轮询 IPC 吞吐，不代表硬件 DRAM 带宽、PMU 精度或通知唤醒延迟。校验失败时断言终止。

## 覆盖范围

- ring 满时丢新记录、物理环绕、uint64 游标溢出、恶意游标、只读映射与 memfd 封印。
- 逐事件复用换算、不同长度时间窗口、有效零流量、异常时间、读失败与基线恢复、可选峰值、扫描窗口偏移；诊断开启和关闭两条路径均检查。
- 自定义事件的整数溢出、负数、无效缩放、额外字段、超长行、重复事件与不完整/混合类别；同时验证完整 64 位 config，以及不同 config1/config2 不被误判为重复。
- FD 传递与 CLOEXEC、非法协议帧、额外 FD 回收、FD 泄漏、四个并发客户端、慢消费者隔离、批量与零拷贝、幂等 START/STOP、重新订阅。
- 从工具目录之外启动服务包装脚本、拒绝重复实例、退出后清理子进程及 socket。
- SIGKILL 后排空队列并返回 EPIPE、SIGTERM 删除 socket、第二个实例拒绝占用同一路径。
- 进程暂停后恢复时保持完整目标等待间隔、LATE/missed_ticks、JSONL 解析与追加、拒绝诊断路径符号链接。

## 性能判断

当前数据通路适合低频 PMU 采样与最多 64 个订阅：硬件扫描共享一次，各 ring 独立丢样，消费接口批量处理且无需 syscall。通知的 seq_cst 发布与 arm/recheck 保持配对；无等待者时只读取通知标志，零数量消费不写共享游标。

关闭诊断时每轮只读取两次时钟，跳过逐计数器诊断快照维护。启用诊断仍会增加格式化和文件 I/O，应按排查需要开启。

未绑定 CPU、未隔离其他负载时，ring 基准随调度和 NUMA 放置明显波动。基准用于验证传输和观察开销，不据此给出稳定提升百分比，也不能代表 PMU 测量精度或端到端唤醒延迟。进一步调优需测量真实 perf read 和调度开销，再决定 CPU/NUMA 绑定或采样间隔。

## 迁入前的硬件对照记录

以下记录来自原 `dram-bw` 目录的 2026-10-09 验证，环境为 x86_64、Linux 5.15.0-139-generic、GCC 9.4.0、Clang 10.0.0。原始数据未随源码迁入，这些数值不代表每次 CI 或迁入后的重新测量。

原环境完成了真实硬件对照：用户以 `sudo make server SERVER_REFERENCE=1` 启动服务和 Linux perf，普通用户执行采集。普通用户直接打开 PMU 仍受权限限制；真实采样没有使用 mock 后端。

双路 EPYC 7532、16 个 DF 事件、1 秒采样；在背景流量、单节点和双节点内存负载下，先测服务单独运行，再做三轮与 perf 同时采样的对照。每段 30 个样本，剔除边界后比较约 25–26 秒的加权均值：

| 新增测试负载 | 三轮汇总相对差异 | 单轮最大绝对差异 |
|---|---:|---:|
| 无（背景流量） | +0.383% | 1.687% |
| node0 | -0.050% | 0.178% |
| node1 | -0.115% | 0.302% |
| 双节点 | -0.128% | 0.260% |

480 条服务样本全部有效，dropped、WINDOW_SKEW、LATE 均为 0；最长扫描 60.021 ms。事件运行比例中位数由服务单独运行时约 50%，降至同时运行 perf 时约 25%。新增负载的本地 NUMA 页占比至少 99.23%。

机器有其他活跃任务，因此无新增负载并非真正空闲；不同时间的吞吐不用于估计工具开销。此结果验证当前配置下与独立 perf 的一致性，不能证明绝对物理精度，也不保证单个 1 秒样本或更短窗口的误差。

离线按相同汇总时段估算后，稳定负载的最大绝对差异为约 0.321%，与原结果接近；背景流量中原先 +1.687% 的那一轮变为约 -0.064%。对齐按秒内速率恒定进行分摊，不能恢复秒内突发。两边独立定时产生的窗口相位差不代表 ring 传输延迟。

生成的原始数据、逐轮报告、运行环境快照和构建产物不纳入源码仓库；这里保留验证结论和复现方法。

自定义事件的物理含义、CPU 热插拔、长期负载行为、systemd 与 Docker 部署未在该次硬件验证中覆盖。

## 容器真实 PMU 验证（2026-10-09）

在双路 AMD EPYC 7532、Linux 5.15.0-139-generic 上，使用 `deploy/services.yaml` 构建的 `project-alpha-dram-bw:local` 镜像进行了真实硬件测试。通过独立 Compose 项目覆盖容器名和 socket 目录，启用 `privileged`，使用 `--backend amd-rome --interval-us 100000` 并开启诊断；保持默认私有 PID/IPC namespace、`network_mode: none`，只绑定 `/sys:ro` 和 socket 目录。没有启动 Tetragon，也没有停止原有宿主机 daemon。

容器进程以 `0:1005` 运行，宿主机客户端 UID/GID 为 `1005:1005`，通过 `0660` socket 读取样本。确认容器持有 16 个 `perf_event` FD，诊断覆盖 `amd_df` 的 CPU 0、32，每个 socket 八个通道；所有 11520 条逐计数器记录都有非零计数增量，读取错误为 0。

每阶段读取 120 条样本，去除前后各 10 条后，按实际采样间隔计算约 10 秒稳定窗口的加权平均。负载为绑定物理核的 `perf bench mem memcpy`，每进程两个 256 MiB 缓冲区，最多使用四个核、约 2 GiB 内存；记录的匿名页全部位于相应本地 NUMA 节点。

| 阶段 | 负载 CPU | 总带宽（GB/s） | socket 0 / socket 1（GB/s） |
|---|---|---:|---:|
| 背景流量 | 无新增负载 | 0.399 | 0.264 / 0.135 |
| node0 | 1、17 | 58.735 | 58.143 / 0.592 |
| node1 | 33、49 | 60.203 | 2.277 / 57.926 |
| 双节点 | 1、17、33、49 | 117.293 | 59.338 / 57.955 |
| 撤销负载 | 无新增负载 | 2.933 | 2.733 / 0.200 |
| 容器重启后 | 无新增负载 | 3.512 | 2.930 / 0.582 |

720 条样本全部有效且不含 MOCK；dropped、LATE、WINDOW_SKEW 和读取错误均为 0。全部样本 flags 为 84，即 MULTIPLEXED、TOTAL_ONLY、PEAK_UNKNOWN；Rome 只提供总带宽，未设置峰值时利用率为 NaN，均符合预期。计数器运行比例中位数约 50.01%，最长扫描 0.415 ms。正常停止删除 socket，重启后普通用户可重新连接并获得有效样本；测试结束已移除测试容器和负载进程，原有 daemon 与 socket 保持运行。

本次验证了容器访问真实 PMU、负载响应、跨容器 socket/共享内存通信及正常启停，不包含独立 `perf stat` 精度对照或强制终止后的恢复。背景有其他任务，各阶段背景流量并非恒定，也不将本次吞吐视为硬件峰值。

原始 CSV、逐事件诊断、NUMA 页分布、镜像 ID、测试脚本及结果保存在本次开发机的 `ctools/dram-bw/results/container-pmu-20261009-907_u777/`，不纳入版本管理。

## 重跑硬件对照

按 [README 的 perf 对照步骤](../README.md#辅助脚本) 启动服务和采集程序，测量口径及窗口限制见 [技术参考](reference.md#perf-对照细节)。

`results/`、`build*/` 和 Python 缓存已加入 `.gitignore`。需要保留某次实验时，将这些数据单独归档；`make clean` 只删除所选构建目录，不删除测量结果。
