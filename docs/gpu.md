# GPU 管理

进入节点工作台的「GPU 管理」，查看物理 GPU、显存、温度、当前进程和历史占用。

## 采集与归属

worker 服务每 15 秒调用 `nvidia-smi -q -x`，并读取宿主机 `/proc` 的 PID 启动时间、UID 和 cgroup。无需 root、Tetragon 或浏览器在线；节点须安装 NVIDIA 驱动和 `nvidia-smi`，服务用户须能读取相关 procfs 信息。支持 NVIDIA 物理卡视角，MIG 卡按物理 GPU 汇总。驱动不提供的字段显示「—」，采集失败显示错误并保留上次成功数据。

容器 ID 从 cgroup 提取，用户归属来自节点登记的 `owners` 表，容器名称优先使用管理记录，其余通过 `docker ps` 补充。宿主机进程标记 `host:用户名`；未归属容器、不可读进程明确标注，不猜测使用者。历史记录保存采集时的归属，之后修改容器归属不会重写过去的时长。

采集同时查询节点服务配置的 rootless Docker 管理 socket，识别该引擎下的容器并补充名称。共用 `docker-rootless` 服务账号的容器统一归类为 `rootless Docker`，详情仍显示各自容器名，避免每个临时容器生成一条未归属排行。共享引擎无法凭进程 UID 反推发起请求的平台用户；已有明确登记归属时仍以登记为准。对于个人 rootless Docker，若 cgroup 用户 slice 与进程 UID namespace 的 root 映射一致，则显示 `rootless Docker · 宿主机用户名`（未知账号显示 UID）。无法读取引擎或 procfs 时保留已知信息并明确标注；不将 rootful Docker 的 userns-remap 当作 rootless。已有历史标签保留，新的分类从部署后的采样开始生效。

受管理的 rootless 业务 socket 经创建来源审计代理转发。创建成功后，代理将内核提供的连接 PID/UID、进程启动时刻、来源容器完整 ID 和新容器 ID 保存到 root 所有的记录中；GPU 采集查询管理接口，将来源容器与节点当前 `owners` 登记对应，直接登记的新容器归属仍优先。只认代理记录，不采信请求头、容器 label 或环境变量里的用户名。外层容器多人共用时，只能归属到该容器登记用户，不能区分其中的自然人；未登记、来源不可读、绕过代理或启用前创建的容器继续显示 `rootless Docker`。来源容器被取消归属后，不再推定其新采样归属；已经保存的 GPU 历史不重写。审计接口不可读会显示警告，GPU 采集继续。

未归属容器在设备摘要、历史图例、占用色带和时长排行中显示为「未归属容器 · 容器名」，不同容器分别展示，同容器多个进程去重。名称暂不可读时显示容器短 ID。

「进程已运行」从 `/proc/<pid>/stat` 的启动时间计算，表示整个进程的年龄，不表示进入 GPU 的时间。设备「未发现进程」只说明采样时未观测到 GPU 进程，不承诺显存完全空闲或计算任务一定可调度；计算禁用和 MIG 模式单独标注。

NVIDIA 字段和限制参见 [nvidia-smi 官方文档](https://docs.nvidia.com/deploy/nvidia-smi/index.html)。

## 72 小时窗口

- 每卡按 UUID 记录两次成功采样之间的区间，以前一次观测的利用率和用户归属估计区间状态。超过 30 秒、采集失败、重启或设备消失均中断连续性，不补算缺测时间。
- 同一用户在同一张 GPU 的多个进程只计一次，多张卡相加。共享 GPU 的每个用户分别累计完整时长；不按 GPU 利用率加权，也不将共享时间平分。未识别、未归属和宿主机进程各自标注。
- 这是采样估计，短于 15 秒的作业可能漏采。累计值是 GPU·小时，不是精确 CUDA kernel 计算时间。
- 数据在 worker SQLite 中持久化，采集时（包括失败时）清理超过 72 小时的区间。SQLite 复用已释放的页面。历史查询也限定最近 72 小时；重启后保留已有记录，但不统计停机时间。
- 每卡曲线支持 1h / 6h / 24h / 72h、放大缩小（最小 15 分钟）、时间平移、采样口径和跟随实时。服务端最多返回每卡 864 个时间桶，必要时自动增大口径。曲线为有效样本的时间加权平均，缺测桶留空；色带按桶内实际观测占用时长调节深浅。
- 单用户区间使用该用户的固定颜色，多用户共享或粗口径内发生用户切换时曲线为灰色，并分别展示用户色带。曲线是整卡利用率，不能解释为某位用户自身的算力利用率。
- 下方用户总时长始终统计本节点最近 72 小时，不受曲线缩放影响。前端轮询替换当前快照，离开页面后停止轮询。

## 接口与升级

`GET /api/gpu/overview?hours=6&step=60` 提供当前设备、进程、曲线和最近 72h 用户总时长。`hours` 为 0.25–72，`step` 为 15–3600 秒，可用 `end`（Unix 秒）平移。经总控的节点认证代理调用，所有已认证工作台用户可读，无 GPU 修改接口。

worker 的 GPU 历史保存在 `gpu_intervals`，到期索引用于清理；control 和 registry 不建立此表。数据库随发布更新原地升级，支持范围和故障恢复见[更新器说明](updater.md)。

验证：`go test -race ./internal/gpu ./internal/app ./internal/platform ./internal/cluster ./cmd/alpha-updater`；浏览器回归 `python3 tests/test_gpu_browser.py` 使用模拟 GPU 数据验证布局和交互。

真实设备验证可显式运行 `PROJECT_ALPHA_TEST_GPU=1 go test ./internal/gpu -run TestLiveNVIDIA -v`。可用 `PROJECT_ALPHA_GPU_TEST_DATA_DIR` 指定只读检查归属的现有数据目录；测试不启动 GPU 负载。
