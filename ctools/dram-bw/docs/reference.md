# 测量与协议参考

构建与部署见 [README](../README.md)，参数和客户端接口见 [命令与客户端 API](usage.md)。本文保留测量口径、状态位、数据通路及 perf 对照的技术细节。

- [测量口径](#测量口径)
- [时间与状态标志](#时间与状态标志)
- [数据通路与开销](#数据通路与开销)
- [诊断与自定义事件](#诊断与自定义事件)
- [perf 对照细节](#perf-对照细节)

## 测量口径

结果覆盖所选计数器对应的宿主机内存流量。START/STOP 控制各客户端订阅；所有订阅共享一次硬件扫描。不能从这些 socket 级计数器推算单个容器的独占带宽。

Rome 后端读取 `amd_df/cpumask` 的全部代表 CPU，每个代表 CPU 打开八个通道事件。事件为 `0x07 + 0x40 * channel`，umask 为 `0x38`，每计数 64 字节，依据 [Linux perf Zen 2 事件表](https://github.com/torvalds/linux/blob/v6.12/tools/perf/pmu-events/arch/x86/amdzen2/data-fabric.json)。事件编码使用 [AMD uncore 驱动](https://github.com/torvalds/linux/blob/v5.15/arch/x86/events/amd/uncore.c) 的 config 布局；例如 `0x107` 的高事件位放在 config 的 32 位起。

该后端仅提供总带宽：`read_bytes_per_sec`、`write_bytes_per_sec` 为 NaN，设置 `DBW_SAMPLE_TOTAL_ONLY`。八个独立事件在有限的硬件计数器上复用，设置 `DBW_SAMPLE_MULTIPLEXED`。

每个事件使用自己的 perf 时间增量：

```text
estimated_bytes = delta_count * bytes_per_count * delta_enabled / delta_running
bytes_per_sec = estimated_bytes / (delta_enabled / 1e9)
              = delta_count * bytes_per_count * 1e9 / delta_running
total_bytes_per_sec = sum(bytes_per_sec)
utilization = total_bytes_per_sec / peak_bytes_per_sec
```

`enabled/running` 的含义见 [perf_event_open](https://man7.org/linux/man-pages/man2/perf_event_open.2.html)。这些速率是复用估计值；短窗口和突发负载可能产生误差。任何通道读取失败、没有运行时间或时间增量异常，整个样本标记 INVALID 并返回 NaN。有效时间窗口内的零计数是有效的 0 B/s。读取失败后的第一次成功读取仅重建基线，下一次才恢复计算。

`--peak-gbps` 必须与计数器覆盖范围一致，可按实际通道配置与内存速率计算，或采用相同覆盖范围的 STREAM 实测值。不提供正数峰值时 Rome 自动估算；不适用估算的后端利用率为 NaN，设置 PEAK_UNKNOWN。利用率不截断到 1。

### 参考峰值自动估算

`--peak-gbps` 省略或为 `0` 时，内置 Rome 后端在启动时估算一次，不运行内存压力测试。正数手动值直接使用。配置文件仍优先于命令行；其中 `0` 同样表示自动。估算不改变 PMU 流量的计算，只提供利用率分母。

计算公式为 `Σ(通道速率 MT/s × 10^6 × 数据位宽 / 8)`，结果使用十进制 GB/s。DDR 的 MT/s 已含双沿传输，不再乘 2；64 位数据总线不包含 ECC 校验位。Rome 每个 DF 实例对应八个通道，通道上限来自后端实际枚举的 PMU 范围，不按 CPU 线程数或 NUMA 节点数计算。[AMD Rome 规格](https://www.amd.com/content/dam/amd/en/documents/products/epyc/amd-epyc-7002-series-datasheet.pdf)给出的每路上限是八通道 DDR4-3200，即 204.8 GB/s。

读取 `/sys/firmware/dmi/tables/DMI`，不依赖 `dmidecode` 外部命令：

1. 使用 SMBIOS Type 17 的已安装 DDR4 内存信息，优先取 **Configured Memory Speed**，支持扩展速率字段，忽略空槽。配置速率缺失时取额定速率，仍缺失时假定 3200 MT/s；数据位宽未知时假定 Rome 的 64 位，并在日志注明假设。
2. Type 37 完整映射所有已安装 DIMM 时，每个实际占用通道只计算一次，同通道多条 DIMM 取最低速率，再按通道求和，来源为 `smbios-channels`。
3. 缺少完整通道映射时，假设均衡插槽布局，通道数取 `min(已安装 DIMM 数, Rome 通道上限)`，速率取所有 DIMM 的最低值，来源为 `smbios-rome`。同通道多 DIMM、不均衡插槽布局可能导致误差；不从厂商自定义槽位名称猜测通道。
4. 固件缺失、不可读或无效时，来源为 `rome-spec`，假设所有 PMU 覆盖的通道满配 DDR4-3200。双路完整覆盖时为 409.6 GB/s，实际未满配或降频时会高估。

这是**理论峰值估算**，不是 STREAM 实测可持续带宽，不额外套用固定效率系数。受固件质量、插槽布局和实际负载影响，实测峰值可能更低。日志显示数值、来源和假设，诊断 session 记录 `peak_bytes_per_sec`、`peak_source`、`peak_detail`；客户端 `dbw_get_stats()` 返回采用的峰值，样本带 `PEAK_ESTIMATED`。如需以实测可持续峰值计算利用率，请提供相同采集范围的手动值。

普通服务用户通常无权读取原始 SMBIOS，会使用 `rome-spec`；现有 privileged Compose 部署可通过只读 `/sys` 读取宿主机信息，无需为估算扩大常驻服务权限。自定义 `--events` 的范围无法自动确认，mock 也不代表真实内存，两者均需手动设置正数，否则保持 `PEAK_UNKNOWN`。

## 时间与状态标志

使用一次性 timerfd，每轮采集、诊断和分发结束后等待完整的目标间隔。实际更新间隔包含处理和调度开销，不承诺固定频率或硬实时延迟。

`timestamp_ns` 是宿主机 CLOCK_MONOTONIC 的扫描结束时间；`interval_ns` 是两次扫描结束的墙上时间差。启用 time namespace 的容器不要直接用自身 monotonic 时间相减。各通道顺序读取，当前或上次扫描耗时超过目标间隔的 10% 时设置 WINDOW_SKEW。

`missed_ticks = max(floor(interval_ns / target_delay_ns) - 1, 0)`，按 uint32 饱和；非零时设置 LATE。这表示额外跨过的完整目标间隔。

`flags` 是位集合，应按位检查：

| 标志 | 数值 | 含义 |
|---|---:|---|
| MOCK | 1 | 显式模拟数据 |
| INVALID | 2 | 带宽无效；同时包含原因位 |
| MULTIPLEXED | 4 | 至少一个事件发生复用 |
| LATE | 8 | 跨过额外的完整目标间隔 |
| TOTAL_ONLY | 16 | 仅提供总带宽 |
| WINDOW_SKEW | 32 | 当前或上次扫描耗时超过目标间隔 10% |
| PEAK_UNKNOWN | 64 | 无可用的利用率参考峰值 |
| PEAK_ESTIMATED | 128 | 利用率分母使用自动估算的理论峰值 |
| READ_ERROR | 256 | perf 读取失败或长度异常 |
| NO_BASELINE | 512 | 仅重建读取基线 |
| TIME_BACKWARDS | 1024 | enabled/running 累计时间回退 |
| NOT_ENABLED | 2048 | enabled 增量为零 |
| NOT_RUNNING | 4096 | running 增量为零 |
| RUNNING_GT_ENABLED | 8192 | running 增量超过 enabled 增量 |
| ZERO_INTERVAL | 16384 | 两轮扫描结束时间相同 |

C 常量均带 `DBW_SAMPLE_` 前缀。原因位按全部通道合并；PEAK_UNKNOWN、PEAK_ESTIMATED、LATE、WINDOW_SKEW 本身不使带宽失效。

## 数据通路与开销

```text
客户端 -- Unix SOCK_SEQPACKET: OPEN / START / STOP --> 守护进程
       <-- SCM_RIGHTS: 只读 data memfd、可写 cursor memfd、通知 socket
       <-- mmap ring: 64 字节记录
```

- 每客户端独立 SPSC ring，容量 2–65536 的二次幂，最多 64 个连接。满队列丢新样本并增加该客户端的 dropped，不阻塞其他订阅，不覆盖已借出的记录。
- `peek`、`consume`、`read_batch`、`get_stats` 无系统调用、锁或内存分配。批量复制最多两次 memcpy；零数量 consume 不写共享游标。
- head/tail 与通知标志按 64 字节 cache line 分离。通知采用“先 arm 再复查”的同步；生产者仅在存在等待者时修改通知标志并发送非阻塞 datagram。忙轮询消费者无需通知 syscall。
- data memfd 只读并封印，cursor memfd 禁止调整大小。守护进程使用私有容量和 head 校验共享 tail，异常客户端被断开。
- 守护进程退出后可排空已入队记录；随后 `dbw_wait` 返回 EPIPE。SIGKILL 也可通过控制 socket 识别。控制收发各有 5 秒超时；失败且关闭控制通道的句柄必须重建。
- 关闭诊断时，每轮只取扫描开始/结束两次时间戳，不维护逐计数器诊断快照。每个事件仍需要一次 perf read，每个运行订阅需要一次记录写入。

共享一次 PMU 扫描并分发至独立 ring，适合当前最多 64 个连接的规模。进一步优化应先测量真实 PMU read、调度与跨 NUMA 访问的占比；CPU 绑定、NUMA 放置可按部署环境验证。缩短采样间隔受复用精度约束，不能仅凭 ring 吞吐决定。

`make bench` 使用两个进程、真实 FD 传递与公开 API；每种模式传输 200 万条并逐条校验内容。它衡量忙轮询下的共享内存传输，不覆盖 PMU 准确性、通知延迟或定时成本。当前验证范围见 [验证说明](validation.md)。

## 诊断与自定义事件

`--diagnostics FILE` 追加 JSONL，新建权限为 `0640`，仅接受普通文件且不跟随符号链接。每次进程启动写 `session`；每轮写逐事件 `counter` 和聚合 `sample`。逐事件记录包含 PMU、CPU、config/config1/config2、bytes_per_count、读取时间、previous/current/delta 三元组（count、enabled_ns、running_ns）、运行比例、原因与 errno。不可计算的 delta 为 JSON null。

格式化、写文件和 flush 在全部计数器读取结束后进行，仍会增加下一轮更新间隔；仅在排查时开启。写入失败会报错并关闭日志，容量由部署方管理。

`--events FILE` 格式见 [`examples/events.conf`](../examples/events.conf)。每行七列：

```text
PMU CPU KIND CONFIG CONFIG1 CONFIG2 BYTES_PER_COUNT
```

CPU 为非负十进制整数；三个 config 为无符号 64 位十六进制数；bytes 为有限正数；KIND 为 read/write 或 total。禁止混用 total 与 read/write，禁止重复 PMU/CPU/config/config1/config2 组合。每行少于 1023 字节（含注释和换行），`#` 开始注释。整个文件校验通过后才打开 perf 事件。

自定义列表需要覆盖全部目标通道与代表 CPU；程序无法验证其物理覆盖范围或事件含义。CPU/内存拓扑变化后重启守护进程重新枚举。

协议 v1 使用本机字节序与 64 位 ABI，适用于同一内核的宿主机和容器。控制帧固定 24 字节，保留字段为零；非法帧、版本或额外 FD 会被拒绝。

## perf 对照细节

先按 [README 中的步骤](../README.md#辅助脚本) 启动 `sudo make server SERVER_REFERENCE=1`。以下命令均在 `ctools/dram-bw/` 中运行。

这会同时启动 `perf stat`，采用 perf 自己的 Zen 2 命名事件
`dram_channel_data_controller_0` 至 `7`，覆盖 `amd_df/cpumask` 的全部代表 CPU，
逐 CPU、逐通道输出，窗口为 1 秒。perf 使用默认复用缩放，不将八个事件放入同一组。
本机 perf 5.15.178 已提供这些事件；不支持这些别名或控制接口时启动会直接报错。
perf 初始保持 disabled；测量程序通过会话内 `perf-control.fifo` 写入 `enable\n` / `disable\n`，
并从 `perf-ack.fifo` 读取 `ack\n`（perf 5.15 还带有一个尾随 NUL 字节），即可在不再次 sudo 的情况下切换参考采样。
同一时刻只能有一个控制程序读取 ACK。原始参考输出为会话内的 `perf.csv`，错误输出为 `perf.stderr`。

服务 READY 后可用普通用户运行已提供的对照脚本（需要 `perf`、`taskset` 和 Python 3）：

```bash
python3 scripts/compare_perf.py --output results/perf-comparison --solo
```

输出目录必须尚不存在。默认每段采集 30 个样本，丢弃两端边界；`--solo` 先测服务单独运行，
随后做三轮无额外测试负载、node0、node1、双节点对照，约 9 分钟。
脚本在每节点最多四个物理核上运行 `perf bench mem memcpy`，峰值约 4 GiB 工作集，
在施加 CPU 亲和性后首次触页，并保存 NUMA 页分布供核对。测量结束自动停止自己创建的负载，
关闭 perf 参考计数，保存客户端 CSV、服务诊断、perf 原始 CSV、各段窗口及结果 JSON。
本脚本针对当前双节点 Rome 机器；用于其他拓扑时应先核对节点和 CPU 配置。

对照时应先测服务单独运行，再测两者同时运行；后者增加 PMU 复用竞争。
在无测试负载、单 NUMA 节点和双节点稳定内存负载下，分别比较多轮、至少 20 秒的稳定窗口，
跳过启停边界，报告相对差异、无效比例、通道运行比例和扫描耗时。
perf CSV 的计数已经缩放，每通道以 `count * 64 / interval_seconds` 换算 B/s，再对通道和 socket 求和，
不可重复缩放。`current.json` 中的 perf 启动时刻只是进程启动边界，并非 perf 的精确内部计时起点，
不能用于宣称逐个 1 秒样本严格对齐。

这种对照可以检验本程序与独立工具的采集、事件编码及汇总是否一致，但两者共享同一硬件 PMU，
一致性不能证明绝对物理精度。STREAM 的算法字节数还受缓存命中、写分配等影响，不能直接作为
DRAM 总流量的真值。事件依据见 [Linux Zen 2 事件表](https://github.com/torvalds/linux/blob/v6.12/tools/perf/pmu-events/arch/x86/amdzen2/data-fabric.json)，
控制接口及输出格式见 [perf stat 文档](https://github.com/torvalds/linux/blob/v5.15/tools/perf/Documentation/perf-stat.txt)，
STREAM 计数口径见 [STREAM 参考说明](https://www.cs.virginia.edu/stream/ref.html#counting)。
