# 命令与客户端 API

以下命令均在 `ctools/dram-bw/` 中运行。构建、模拟服务和部署步骤见 [README](../README.md)。

## `dram-bwd`

宿主机上的服务端。默认前台运行，日志写到 stderr；它不自行转入后台，也不把样本打印为 CSV。
第一个客户端开始订阅后启动采样，最后一个订阅停止或断开后暂停采样，进程继续等待连接。

```bash
./build/dram-bwd --help
./build/dram-bwd [选项]
```

| 选项 | 默认值 | 含义 |
|---|---|---|
| `--socket PATH` | `/run/dram-bw/control.sock` | Unix socket；父目录须存在，路径不能被占用 |
| `--backend auto\|amd-rome\|mock` | `auto` | 自动识别 Rome、指定 Rome，或显式模拟 |
| `--events FILE` | 无 | 自定义 PMU 事件文件，配合 `--backend auto` 使用 |
| `--interval-us N` | `100000` | 每轮处理结束后的等待微秒数，范围 100–10000000 |
| `--peak-gbps N` | 无 | 计算利用率用的正数参考峰值，单位十进制 GB/s |
| `--socket-mode OCTAL` | `0660` | socket 文件权限，八进制 |
| `--allow-uid HOST_UID` | 无额外 UID 限制 | 限制可连接的宿主机视角 UID；文件权限仍生效 |
| `--diagnostics FILE` | 关闭 | 追加逐事件和聚合样本 JSONL，增加文件 I/O |
| `-h` / `--help` | — | 显示帮助并退出 |

内置硬件后端适用于 AMD EPYC Rome（Family 17h / Model 31h，包括 EPYC 7532）。
其他 CPU 需要自行核对事件后提供 `--events`，格式见 [事件配置参考](reference.md#诊断与自定义事件)。
硬件初始化失败会报错退出，不会自动切换到 mock。

间隔包含额外处理和调度开销，不是精确固定周期。Rome 事件会复用，实际采样建议从 100 ms 或更长间隔开始。
正常退出或帮助返回 0，运行失败返回 1，参数错误返回 2。

## `dram-bw-consume`

日常查看和导出数据的客户端。需要先启动服务端并取得 socket 访问权限。

```bash
./build/dram-bw-consume --help
./build/dram-bw-consume /run/dram-bw/control.sock       # 默认读取 100 条
./build/dram-bw-consume /run/dram-bw/control.sock 10    # 读取 10 条
./build/dram-bw-consume /run/dram-bw/control.sock 100 > samples.csv
```

语法：`dram-bw-consume [选项] SOCKET [NUMBER_OF_SAMPLES]`。socket 必填；样本数范围 1–10000000，包含无效样本。
客户端沿用服务端的采样间隔。读满后停止自己的订阅并退出，服务端继续运行。
stdout 只输出 CSV，stderr 输出诊断和结束统计，因此 `> samples.csv` 不会把日志混入数据。

| CSV 列 | 含义 |
|---|---|
| `sequence` | 服务端样本序号 |
| `host_monotonic_ns` | 宿主机单调时钟下的扫描结束时间，不是日历时间 |
| `interval_ns` | 相邻扫描结束的时间差，单位 ns |
| `read_GBps` / `write_GBps` / `total_GBps` | 读、写、总带宽；1 GB/s = 10⁹ B/s |
| `utilization` | 总带宽 / 参考峰值；0.5 表示 50%，可能大于 1 |
| `flags` | 十进制状态位集合，按位判断，详见 [完整标志表](reference.md#时间与状态标志) |
| `missed_ticks` | 额外跨过的完整目标间隔数 |

**Rome 只提供总带宽**，所以读写两列为 `nan`。未设置 `--peak-gbps` 时利用率为 `nan`。
`nan` 表示未知或无效，不能当作零；硬件读取异常会设置 `INVALID` 标志。
正常读完或帮助返回 0，连接/读取失败返回 1，参数错误返回 2。

## `libdram_bw.a` 和 `libdram_bw.so`

两份库都用于编写客户端，不是可执行程序，也不会自动启动 daemon。
头文件是 [`include/dram_bw.h`](../include/dram_bw.h)，完整示例是 [`examples/consume.c`](../examples/consume.c)。
下面用现有示例演示两种链接方式；自己的程序可替换源文件和输出名。

```bash
# 静态链接客户端库：运行时不需要 libdram_bw.so
cc examples/consume.c -Iinclude build/libdram_bw.a -lm -o build/client-static
./build/client-static /tmp/dram-bw-demo/control.sock 10

# 动态链接客户端库：运行时须让 loader 找到 .so
cc examples/consume.c -Iinclude -Lbuild -ldram_bw -lm -o build/client-shared
LD_LIBRARY_PATH="$PWD/build${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" \
  ./build/client-shared /tmp/dram-bw-demo/control.sock 10
```

运行命令需要 [模拟示例](../README.md#先跑通模拟示例) 中的 daemon 仍在运行。`client-static` / `client-shared` 是示例额外生成的文件，不属于默认 make 产物。

| 调用 | 用途 |
|---|---|
| `dbw_open` | 连接并创建句柄，初始 STOPPED；容量 0 表示默认 4096 |
| `dbw_start` | 开始订阅；加入已有采样时跳过已开始的区间 |
| `dbw_wait` | 1 有数据，0 超时或停止，-1 错误；忙轮询可不调用 |
| `dbw_read_batch` | 批量复制到应用自己的缓冲区 |
| `dbw_peek` + `dbw_consume` | 零拷贝查看最多两段连续记录，再提交消费游标 |
| `dbw_get_stats` | 查询 produced、available、dropped、state 等独立统计值 |
| `dbw_stop` / `dbw_close` | 同步停止订阅 / 释放句柄；停止后仍可读已入队样本 |

每个句柄限一个线程使用；多线程分别创建句柄。START/STOP 幂等，统计值不是事务快照。
`peek` 借出的内存在 `consume/close` 前有效，提交前不要混用 `peek` 和 `read_batch`。
容量须为 2–65536 的二次幂；一个服务端最多 64 个连接。
API 中带宽单位是 **B/s**，命令行客户端才会转换为 GB/s。
