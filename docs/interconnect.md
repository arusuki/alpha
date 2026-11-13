# 容器互联

总控管理员可以给已接管的 bridge 容器分配固定 IPv4 地址，连接同机或同一二层内网上不同 worker 的容器。连接可以跨使用者。应用监听互联 IP 或所有接口即可使用授权端口，NCCL 的地址选择、端口和环境由应用配置。

## 准备 worker

使用本机 rootful Linux Docker Engine 28 或以上版本、支持 `--gw-priority` 的 Docker CLI、Docker 的 iptables 后端和 systemd 管理的 `docker.service`。宿主机需要 iproute2、iptables/ip6tables 及其 restore 命令、nsenter、ping、sudo 和 visudo。TCP 服务诊断使用宿主机的 OpenBSD netcat。初始化会绑定 worker 容器设置中的本机 Docker Unix 套接字及 daemon 身份。套接字须由 root 拥有，其实际目录仅由 root 管理。默认路径为 `/var/run/docker.sock`，节点的 `internal_ip` 应为本机接口的 IPv4。跨 worker 的两个内网地址需能直接解析彼此的二层邻居。

在已初始化 worker 数据目录的机器上，从可信发布包运行以下命令，将账户和数据目录替换为实际服务配置。

```sh
sudo ./bin/project-alpha interconnect init --user project-alpha --data-dir /var/lib/project-alpha-worker
```

初始化读取同一发布包的 `bin/alpha-interconnect-helper`，安装到 `/usr/local/libexec/project-alpha/alpha-interconnect-helper`。sudoers 仅允许指定普通用户无参数执行这个安装副本。Docker 的 systemd drop-in 在 daemon 启动前安装默认拒绝保护。管理员升级 helper 时再次执行相同的初始化命令，再在总控选择 **重试应用**；二进制变更会使原策略摘要失效，让状态检查要求重新应用，重新初始化期间保留在线规则。updater 更新发布目录中的二进制。数据库升级入口和支持范围见 [updater 使用说明](updater.md)。

## 配置与状态

1. 在总控侧栏打开 **容器互联 → 节点与接入**。检查 worker 的内网地址、接口、helper 协议和防火墙能力。在接入首个端点前可调整默认地址池 `10.240.0.0/16`。使用边界对齐、至少包含一个 /24 的 IPv4 单播子网。
2. 在 **连接编排** 的容器目录中按名称、使用者、容器 ID 或 IP 搜索，也可按节点筛选。依次点选两个容器作为 A、B；点击端点卡片可指定要替换的一端。未接入的容器会在 **接入并保存连接** 时分配地址。只需分配地址时，在 **节点与接入** 选择 **接入容器**。总控为每个 worker 分配一个 /24，每个端点使用一个独享的 /30 routed bridge。/24 可容纳 64 个端点。地址或路由冲突会显示具体错误。
3. 逐条设置方向、协议、目的端口和启用状态。TCP/UDP 的起始和结束端口构成闭区间，留空表示全部端口。规则取并集。单向规则允许回包，撤销规则会阻断失去授权的已有连接。专用网络默认拒绝，原网络继续使用自己的访问策略。
4. 保存后在 **已有连接** 核对两端应用版本。若部分容器已成功接入但后续操作失败，目录和草稿会保留已分配的地址，处理错误后可继续保存。编辑期间配置版本发生变化时，刷新并选择 **重载最新规则**，核对后保存。连接上的 **诊断** 从指定源容器的网络 namespace 发起一次绑定互联源 IP 的 TCP 连接，使用宿主机程序。诊断需要目的端口有实际服务监听。
5. 节点离线时，总控保留待下发版本并自动重试，两端都确认后才完成撤销。配置或宿主机报错时，处理节点错误后选择 **重试应用**。先删除端点关联的连接，等待确认，再退出端点并释放地址。

容器停止时保留端点地址，运行中的其他容器继续按已授权规则通信。停止期间可以撤销连接。容器再次启动时，worker 按最新规则恢复其互联。worker 重启或 Docker 事件流重连时会重新核对持久配置。

worker 使用完整容器 ID、原配置指纹、daemon 身份及独立的网络附加检查。专用访问链绑定两端 IP、收发接口、二层来源和 conntrack 方向。跨节点的 /32 宿主机路由通过对端内网地址转发。附加网络的网关优先级低于原有网络，容器的 /32 路由指定互联源地址。原 bridge 继续提供默认网关和已发布的 SSH 入口。[Docker 网络附加参数](https://docs.docker.com/reference/cli/docker/network/connect/)

当内网链路或对端 bridge 的 MTU 较小时，防火墙允许当前授权连接的 IPv4“需要分片”错误返回，使 TCP 能发现路径 MTU。错误规则绑定原始连接的地址、协议、目的端口和方向；转发路径还校验接口与来源 MAC。单向授权也支持回包触发的 MTU 错误。缩小或撤销授权时同步移除相应错误规则，已有 conntrack 记录不会保留被撤销的权限。

## 清理

节点更换内网地址、容器转交使用者、解除接管或删除前，先在面板完成互联清理。节点离线时保留其待撤销记录。要卸载 helper，在该 worker 的所有端点退出并确认后运行：

```sh
sudo ./bin/project-alpha interconnect init --uninstall
```

卸载清理本功能的规则链、带归属标记的跳转、sudoers、Docker drop-in、helper 和本机归属记录。

## 开发验证

浏览器回归使用真实总控、临时数据库和两个 worker 测试接口。Linux 网络回归启动两个独立 Docker daemon，在其 namespace 之间创建二层链路。测试需要本机 Docker 权限，两个临时 worker 使用 privileged 模式和私有 cgroup namespace，结束时删除临时容器与网络。以下命令在项目根目录执行。

```sh
python3 tests/test_interconnect_browser.py
docker build -f tests/interconnect.Dockerfile -t alpha-interconnect-test:local tests/
python3 tests/test_interconnect_network.py
```

构建参数 `ENGINE_IMAGE` 可指定包含相同 Docker 二进制的本地镜像。也可通过 `PROJECT_ALPHA_INTERCONNECT_IMAGE` 指定已有测试镜像。镜像需包含 rootful Docker CLI/daemon、Python 3、sudo、iptables、iproute2、nsenter 和 ping。测试的轻量监督进程执行与 systemd drop-in 相同的启动保护命令。

网络回归还会在隔离 worker 内降低内网接口与专用 bridge 的 MTU，验证单向 TCP 授权下双向 2 MiB 传输的数据摘要及本机生成、对端转发的 MTU 错误，并检查端口、方向变更和撤销后旧连接的错误报文被拒绝。

### 隔离环境的性能测量

在 A6000 的同一台 Linux 宿主上运行两个 Docker 28.0.1 daemon，两个 worker 通过专用 veth 模拟同一二层链路。同机 host 基线使用回环，跨 worker host 基线使用上述 veth。每条路径交错测量三轮，每轮运行 3 秒单流 iperf3，并另发 30 个 ICMP 回显请求。表中是三轮的中位数。CPU 为 iperf3 报告的发送端和接收端进程占用，100% 对应一个逻辑核。

| 路径 | 接收吞吐（Gbit/s） | 平均 RTT（ms） | 发送端 CPU | 接收端 CPU |
| --- | ---: | ---: | ---: | ---: |
| 同机 host | 15.06 | 0.026 | 98.5% | 12.1% |
| 同机容器互联 | 15.16 | 0.046 | 98.5% | 14.4% |
| 跨 worker host | 15.94 | 0.031 | 98.6% | 13.1% |
| 跨 worker 容器互联 | 12.74 | 0.060 | 98.5% | 14.6% |

这组测量覆盖内核转发、bridge 和访问控制开销。物理网卡、交换机、RDMA 和 NCCL 训练作业需要在实际集群另行测量。
