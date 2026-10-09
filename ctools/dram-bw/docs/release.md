# dram-bw release 包

在 Linux 宿主机上采集 DRAM 带宽，供本机或容器内的客户端读取。所有命令均在解压后的包目录中执行。

## 包内容

| 路径 | 用途 |
| --- | --- |
| `bin/dram-bwd` | daemon 服务端，读取 PMU 并分发样本 |
| `scripts/server.py` | 前台 server 包装脚本，管理 daemon、访问权限和可选 perf 对照 |
| `bin/dram-bw-consume` | 命令行客户端，输出 CSV |
| `lib/libdram_bw.a`、`lib/libdram_bw.so` | 静态与动态客户端库 |
| `include/dram_bw.h` | 公开 C API 头文件 |
| `systemd/dram-bwd.service` | systemd 服务单元示例 |
| `examples/dram-bw.default` | 服务参数示例 |

程序要求与构建时相同的 CPU 架构及兼容的系统运行库、64 位 Linux 5.1+。运行预编译程序无需 GCC 或 make；server 脚本需要 Python 3。真实采样需要相应的 PMU、驱动和权限；内置后端支持 AMD EPYC Rome。

## 运行

终端一启动 mock server，无需 PMU 权限。包中的可执行文件位于 `bin/`，须显式传入 `--build bin`：

```bash
python3 scripts/server.py --build bin --backend mock --directory /tmp/dram-bw-demo
```

终端二读取 10 条样本：

```bash
./bin/dram-bw-consume /tmp/dram-bw-demo/control.sock 10
```

真实 Rome 采样可改用 `--backend amd-rome`，需要 root 或相应 PMU 权限。通过 sudo 启动时，使用 `--uid` 和 `--gid` 授权原用户访问，例如：

```bash
sudo python3 scripts/server.py --build bin --backend amd-rome \
  --uid "$(id -u)" --gid "$(id -g)" --directory /run/dram-bw
```

Ctrl-C 停止包装脚本创建的进程，保留会话日志。也可直接运行 `./bin/dram-bwd --help` 查看服务端选项，自行准备 socket 父目录及访问权限。

## 链接客户端库

将自己的程序保存为 `client.c`，使用 `#include <dram_bw.h>`。两种链接方式：

```bash
cc client.c -Iinclude lib/libdram_bw.a -lm -o client-static
cc client.c -Iinclude -Llib -ldram_bw -lm -o client-shared
LD_LIBRARY_PATH="$PWD/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ./client-shared
```

库不会自动启动 daemon。API 带宽单位为 B/s，命令行客户端输出十进制 GB/s；数据覆盖宿主机计数器对应的流量，不能归属单个容器。

## 安装到 /usr/local

```bash
sudo install -d /usr/local/bin /usr/local/lib /usr/local/include
sudo install -m 0755 bin/dram-bwd bin/dram-bw-consume /usr/local/bin/
sudo install -m 0755 lib/libdram_bw.so /usr/local/lib/
sudo install -m 0644 lib/libdram_bw.a /usr/local/lib/
sudo install -m 0644 include/dram_bw.h /usr/local/include/
sudo ldconfig
```

常驻服务使用包中的 systemd 单元：先创建 `dram-bw` 系统用户和组，将服务单元复制到 `/etc/systemd/system/`，默认配置复制到 `/etc/default/dram-bw`，按硬件设置参数，再执行 `sudo systemctl daemon-reload` 和 `sudo systemctl enable --now dram-bwd`。服务以 `dram-bw` 用户运行，通过 `CAP_PERFMON` 访问 PMU；普通客户端需要相应的组权限。启动前须准备驱动并停止占用同一路径的前台实例。
