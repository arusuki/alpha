# 增量探索的合并与持久化性能

重复索引、重复合并和未变化节点的序列化可以消除；扫描与保存可以重叠执行。首次读取基线、建立索引、新节点写入和最终事务提交仍需要实际工作。缓存不替代下一次探索的文件系统测量，也不提前宣布尚未持久化的版本已经完成。

## 具体瓶颈与改动

此前 `mergeDirectoryResult` 每次发布都索引整棵结果树、遍历全部节点检查完整性，`storeSnapshot` 又重建当前及历史 inode 索引和历史节点索引。虽然没有重写未变化行，比较、索引和编码仍发生在 `BEGIN IMMEDIATE` 事务内。`Scanner.walk` 的回调还同步等待这些步骤完成。

CPU 与分配剖析确认热点包含 `snapshotNodes`、inode map 构建、路径前缀判断、JSON 编码和 SQLite 参数绑定。当前实现分别处理这些开销：

- [`directory_merge_state.go`](../internal/storage/directory_merge_state.go) 的 `directoryMerge` 在一次探索开始时校验并索引基线，缓存目标外的节点数量、引用、未知状态和 inode 信息。后续 `merge` 只索引、校验目标分支及祖先；容器可写层查询复用这个索引视图。没有外部共享引用或读取错误时，直接复用已扫描节点。
- [`snapshot_writer.go`](../internal/storage/snapshot_writer.go) 的 `snapshotWriter` 保存已提交节点、位置与身份。相同节点和相同身份的完整子树可以跳过；新扫描产生的新对象仍按值比较。身份变化时不能仅凭节点指针跳过。`prepare` 在事务外生成变更行与删除列表，大批量编码最多使用 4 个协程。`store` 每批写入 128 行并复用语句，删除也分批处理。
- [`directory_pipeline.go`](../internal/storage/directory_pipeline.go) 将物理扫描放在生产者协程中，合并、进度回调和发布在调用方协程按顺序处理。邮箱最多保留一份待处理目录观察和一份进度；累积观察可以用新观察覆盖。最终结果到达后跳过冗余的待处理观察，最终提交仍同步完成。
- [`scan.go`](../internal/storage/scan.go) 的 `within` 直接比较路径边界，避免反复拼接 `root + "/"`。这不会改变排除目录和路径归属规则。

```mermaid
flowchart LR
    Scan[物理扫描] --> Mail[有界邮箱：只留最新观察]
    Mail --> Merge[目标分支合并与校验]
    Merge --> Prepare[变更比较与并行编码]
    Prepare --> Tx[单个 SQLite 写事务]
    Tx --> Cache[提交成功后推进缓存]
```

SQLite 使用单连接和立即写事务。增加并行 SQL 写入会在同一连接和写锁上等待，所以并行放在文件系统遍历和独立节点编码上。节点、祖先、元数据、版本、审计和最终任务状态继续原子提交。提交失败不会推进缓存；取消或回调失败会停止并等待扫描协程退出，已提交观察保留。

缓存的作用域是一轮目录 worker。完整基线索引仍需建立一次，独立探索不会复用旧的文件系统测量。目录分析元数据、祖先的子节点数组、新增 inode 记录及实际 SQLite 提交仍有开销；特别宽的父目录仍可能需要复制其子节点数组。

## 基准与验证

```sh
go test ./internal/storage -run '^$' -bench '^(BenchmarkIncrementalScan|BenchmarkDirectoryMerge|BenchmarkSnapshotPrepare)$' -benchtime=10x -benchmem -cpu=4 -count=2
go test ./...
go test -race ./internal/storage -run 'Test(Directory|Incremental|SnapshotWriter|RecordQueries)' -count=1
```

`BenchmarkIncrementalScan` 对同一批 16513 个文件和目录使用相同深度，比较本地统计、增量遍历与合并、以及含节点持久化的增量处理。每次探索重新建立发布器，缓存只在该次探索的检查点之间复用。它包含索引准备与最终节点保存，不包含文件创建、worker 进程启动、快照加载和 HTTP/SSE 传输。发布模式包含首次初始化和后续刷新，首次写入的占比会随迭代数改变，应使用相同迭代数比较。

`BenchmarkDirectoryMerge` 在含 1 万或 10 万个无关节点的基线旁更新一个 32 节点分支，单独测量每次检查点的合并；基线初始化不计入此项。`BenchmarkSnapshotPrepare` 同样只测量索引已建立后的变更比较与编码，不包含 SQL 提交。

本机 AMD EPYC 7532、`GOMAXPROCS=4` 的检查点对照（对照版本为上一轮优化后的实现，固定 5 次迭代）：

| 基线中的无关节点 | 合并优化前 | 合并优化后 | 每次合并分配：前 → 后 |
|---|---:|---:|---:|
| 10000 | 3.68 ms | 0.019 ms | 961 KB → 5.8 KB |
| 100000 | 54.69 ms | 0.030 ms | 7.80 MB → 5.8 KB |

同样条件下，变更比较与编码为 0.087–0.100 ms，分配约 35 KB，不随无关分支从 1 万增长到 10 万而线性增长。数字取决于文件系统缓存、磁盘、CPU 负载和节点形状，不能把检查点微基准当作整个 API 的响应时间。

16513 条目的遍历基准，固定 10 次迭代、运行两组取平均：

| 范围 | 本轮优化前 | 本轮优化后 |
|---|---:|---:|
| 增量遍历与合并 | 230 ms | 172 ms |
| 增量遍历、合并与节点持久化 | 400 ms | 266 ms |
| 含持久化模式每次分配 | 69.3 MB | 46.7 MB |

优化后的同批本地统计约 116 ms。因此，大记录里小分支的重复合并已接近常量成本，整个探索流程仍没有达到纯本地统计的耗时；保留大量新明细、inode 身份和观察元数据需要额外处理与持久化。

回归覆盖扫描与发布重叠、1000 份排队观察合并成最新一份、回调失败时取消并等待扫描退出、跨批次删除和回滚、同一发布器回滚后重试、仅 inode 变化时仍更新记录，以及现有的版本冲突、硬链接、权限、SSE 重建和取消恢复行为。
