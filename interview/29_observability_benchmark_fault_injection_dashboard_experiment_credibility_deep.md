# 十、Observability、Benchmark、Fault Injection、Dashboard 与实验可信度（深度）

这一组问题更像面试里的追问。回答时要承认实验边界，别把单机 smoke benchmark 包装成生产结论。LogServe 当前的实验价值在于验证机制：log-first、replay、redelivery、snapshot、locality scheduling 和 checkpoint cache 都能跑通，并且留下了可复查的结果文件。

## Q706. logstore benchmark 中 append records/s 为什么受 fsync policy 影响巨大？

因为 `fsync` 决定了每次 append 是否要等磁盘把数据刷到稳定存储。

在 LogServe 的 LogStore 里，一次 append 不只写内存。它会写 segment log，也会写 index。`FsyncAlways` 下，append 后会调用 `syncFilesLocked()`，同步 log file 和 index file。这个操作会穿过操作系统 page cache，等待文件系统和存储设备完成刷盘。对单条小记录来说，几百字节的写入并不贵，每条记录都刷一次盘才贵。

`FsyncBatch` 不在每次 append 热路径里 sync，通常等 close 或 rollover 时再同步；`FsyncInterval` 则按时间窗口同步，比如默认 100ms。它们可以把很多条记录合并到一次磁盘 flush 里，所以 append records/s 会高很多。

这里要同时讲清楚代价。`batch` 和 `interval` 的吞吐数字好看，但 durability window 变大了。客户端收到 append 成功后，如果进程或机器马上崩溃，尚未 sync 的记录可能丢失。`always` 慢，但语义更硬。这个 benchmark 的价值在于把吞吐和持久化边界摆出来，而不是给某个 policy 下绝对结论。

## Q707. Read records/s 高是否说明系统整体吞吐高？为什么？

不能这么理解。

LogStore benchmark 里的 read records/s 测的是本地 logstore 从 index 读取 payload 的速度。这个路径通常会受到 page cache、顺序读、payload 大小和 index 命中影响。如果刚 append 完就读，很多数据还在内存缓存里，读出来会很快。

系统整体吞吐要复杂得多。一次真实 task 或 workflow 请求会经过 SDK、gRPC、control、log append、metadata view 更新、worker poll、Python runner、result store、completion log、dashboard view 等路径。LLM task 还会涉及模型加载、checkpoint cache、mock 或 vLLM adapter。

所以 read records/s 高只能说明“日志读路径本身不是当前这个微基准里的瓶颈”。它不能说明 workflow throughput、actor command throughput 或 LLM serving throughput 也高。面试里我会把它定位成存储层指标，不把它外推到整个 runtime。

## Q708. workflow p95/p99 latency 的样本量是否足够？

默认实验里的样本量不够做严肃统计。

`examples/evaluation/benchmark.py` 默认只跑 3 个 workflow。3 个样本下的 p95/p99 基本就是最大值附近。它适合作为 smoke benchmark，能检查 workflow DAG、LLM step、worker 执行、completion 和 replay 是否能跑通；但它不适合写成“系统 p99 延迟为多少”的生产结论。

如果要让 p95/p99 更可信，至少要做几件事：

- 把 `LOGSERVE_BENCH_WORKFLOWS` 提高到几十或几百。
- 多轮重复实验，记录每轮结果。
- 固定机器、worker 数、pool size、checkpoint 大小、fsync policy。
- 区分冷启动和热身后的请求。
- 报告置信区间，或者至少报告多轮最小值、中位值和最大值。

我现在会这样表述：当前 workflow p95/p99 证明链路可运行，能给调试和回归测试用；它还不是一个强统计意义的延迟结论。

## Q709. task throughput 5.17 tasks/s 是瓶颈还是实验参数限制？

更像实验参数和提交方式限制，不应该直接当成系统瓶颈。

当前 `task_throughput()` 是在 Python benchmark 里串行调用 `submit(ping, i)`。它采用“一个请求完成后再提交下一个请求”的方式，没有并发压满 worker。这个数字包含 SDK 同步等待、gRPC 往返、control 写 log、worker poll、Python runner 执行、completion 等端到端开销。

实验 runner 默认 worker capacity 是 1，三台 worker 也是单机进程。即使 worker 本地已经有 executor pool，benchmark 的提交端也没有把并发打满。`ping` 本身又是极小任务，固定开销占比很高。

所以 5.17 tasks/s 的含义是：在这组默认单机参数和串行提交方式下，端到端 smoke workload 的吞吐是这个量级。它不是 LogServe 的理论上限。要找瓶颈，需要并发提交、调大 `LOGSERVE_BENCH_TASKS`、调 worker capacity 和 pool size，再把 latency breakdown 拆开看。

## Q710. actor snapshot replay 1 vs 21 commands 的实验如何设计？

这个实验用两个 Counter actor 做对照。

第一个 actor 开启频繁 snapshot，比如 `snapshot_every=5`。第二个 actor 把 snapshot 间隔设得很大，比如 `snapshot_every=100000`，在当前实验规模下相当于不触发 snapshot。

然后对两个 actor 执行相同数量的 `inc()`，默认是 20 次。执行结束后再调用一次 `get()`，确保 actor 状态走过完整 command 应用路径。接着分别调用 `replay_actor()`，读取 runtime 从 actor stream 和 snapshot store 重建出的统计：

- `full_replay_commands`
- `snapshot_replay_commands`
- `trimmed_replay_commands`

开启 snapshot 的 actor，replay 可以从最近 snapshot 开始，只回放 snapshot 之后的 tail log。没有 snapshot 的 actor，只能从 stream 开始解释命令。已有实验里出现 1 vs 21，说明 snapshot 把恢复要解释的命令数量降下来了。

这个设计好在对照清楚：两个 actor 类逻辑一样，命令数量一样，只改变 snapshot 策略。它测的是 replay work，不是单纯测 `inc()` 函数速度。

## Q711. snapshot-aware retention 中 compactable bytes 如何解释？

`compactable bytes` 表示已经被 logical trim 标记为可压缩的日志字节数。

在 actor 场景里，snapshot 创建后，系统知道 snapshot 之前的一段 command log 已经不再是常规恢复的必需路径。Actor replay 可以从 `ActorSnapshotCreated` 对应的 snapshot ref 开始，再读后面的 tail log。于是控制面会调用 logstore 的 logical trim，把某个 stream 的 trim point 向前推进。

这时 logstore 还没有物理删除 segment。它只是记录“这个 stream 在某个 seq 之前的记录对普通 replay 来说可以跳过”。`compactable_records` 和 `compactable_bytes` 就是这个标记带来的统计量。

它的正确解释是：

- 它不是已经释放的磁盘空间。
- 它是未来 physical compaction 可以考虑回收的空间。
- 它说明 snapshot 和 retention 协作生效了。
- 它还需要配合 snapshot object 的可靠性检查，不能只看到 bytes 就删文件。

面试里可以一句话概括：compactable bytes 是“可回收潜力”，不是“已回收空间”。

## Q712. locality-aware p95 从 305ms 到 205ms，是否有统计显著性？

默认实验下不能说有严格统计显著性。

已有数字显示 `LOCALITY_AWARE` 的 p95 比 `RESOURCE_ONLY` 低，cache hit rate 也更高。这个结果方向合理，因为 locality-aware 更倾向把 `model-A` 请求发给已经缓存 `model-A` 的 worker，减少 cold start。

但默认 `LOGSERVE_BENCH_LLM_REQUESTS` 只有 6。6 个样本算 p95，统计意义很弱。一次调度、一次本机负载波动、一次 Go/Python 调度抖动，都可能影响尾延迟。

我会把这个结果说成“机制验证结果”，而不是“统计显著结论”。如果要回答统计显著性，需要扩大请求数，跑多轮，固定随机或请求序列，分别记录每轮的 p95、p99、cache hit rate 和 cold start count。再用置信区间或非参数检验判断差异是否稳定。

## Q713. resource-only cache hit rate 0.833 是如何产生的？

这个值来自 6 次 LLM 请求里有 5 次命中缓存、1 次没有命中。`5 / 6 = 0.833`。

实验环境里有 3 个 worker。worker-1 预先有 `model-A`，worker-2 有 `model-B`，worker-3 初始没有目标模型缓存。`RESOURCE_ONLY` 主要看 worker 空闲资源，不把模型 locality 当成优先约束。这样在某些请求上，它可能把 `model-A` 请求分给没有 `model-A` 的 worker，触发 cold start。只要 6 次里出现一次这种分配，cache hit rate 就会变成 0.833。

这个数字本身还受请求数量影响。默认只有 6 次请求，所以一次 miss 的权重很大。它适合解释调度机制，不适合作为稳定业务指标。要得到更可靠的 cache hit rate，需要更多请求、更长运行时间，并控制初始 cache 分布。

## Q714. checkpoint cold fetch 1ms 是否能代表真实 checkpoint 场景？

不能。

这次 checkpoint probe 用的是本地文件系统，checkpoint 默认很小，通常是 1 MiB 量级。source dir 和 worker-local cache 都在同一台机器上，复制成本很低，所以 cold fetch 只有 1ms 是合理的。

真实模型 checkpoint 可能是几十 GB，source 可能在对象存储或网络文件系统上。冷启动会受到很多因素影响：

- 对象存储读取延迟。
- 网络带宽。
- 解压或格式转换。
- 本地磁盘写入速度。
- GPU 权重加载。
- vLLM 初始化。
- 显存分配和 KV cache 初始化。

所以这个 probe 只能证明“checkpoint cache 逻辑和指标链路正确”：cold 会 fetch，warm 会 hit，本地 artifact 存在，replay 能读出 fetch/cache 指标。它不能代表真实大模型 cold start 的耗时。

## Q715. mock LLM benchmark 与真实 GPU benchmark 的区别如何向面试官说明？

我会先承认边界：mock LLM benchmark 不测 GPU 性能。

mock LLM 的作用是把 LogServe 自己的 runtime 路径测清楚，包括模型注册、LLM task 提交、worker 调度、checkpoint cache、cache hit/miss 事件、`ModelLoadStarted`、`ModelLoaded`、`LLMCompleted`、ReplayLLM 和 locality ablation。没有 GPU 时，这些路径仍然可以验证。

真实 GPU benchmark 要测的是另一层东西：

- vLLM endpoint 延迟。
- prompt prefill 和 decode 时间。
- token throughput。
- continuous batching 效果。
- GPU 显存压力。
- 多模型切换成本。
- 大 checkpoint 加载时间。
- GPU OOM 和重调度。

所以我不会把 mock 结果说成真实 serving 性能。我会说：mock 实验证明 LogServe 的调度、事件和恢复链路跑通了；接入真实 GPU 后，需要补 token-aware 和 GPU-aware 的 benchmark。

## Q716. fault injection 如何模拟 worker kill？

当前有两种层次。

在集成测试里，很多 worker kill 场景并不直接杀 OS 进程。测试主要通过控制 worker 生命周期、停止 worker 心跳或让任务 lease 过期来模拟 worker 丢失。比如 running task redelivery 测试会让 worker-1 poll 并 start 一个 task，然后等待超过 redelivery timeout，再让 worker-2 poll 到同一个 task。

在脚本层，`scripts/fault_injection.ps1` 会启动 logd/control 进程，再用 `Stop-Process -Force` 停掉 control 或 logd，模拟进程重启。Linux 实验脚本则通过 Go 集成测试覆盖 worker recovery 和 redelivery，再把结果写入 `fault_injection.json`。

如果要更接近真实 worker kill，可以增加一个外部进程级测试：启动真实 `logserve-worker` 进程，让它拿到任务后 `kill -9`，再启动另一个 worker，看任务是否 redelivery，旧 completion 是否被 lease epoch 拒绝。当前测试已经覆盖核心语义，但进程级 kill 还可以加强。

## Q717. poll-before-start task redelivery 是什么场景？

这是 worker 已经从 control poll 到任务，但还没来得及调用 `StartTask` 就挂掉的场景。

普通 redelivery 往往假设任务已经进入 running 状态。poll-before-start 更 tricky：任务已经被某个 worker 拿走，可能从 queue 里移除了，但 control 还没有收到 `TaskStarted`。如果这时 worker 死掉，系统不能让任务永远卡住。

LogServe 的测试 `TestPolledTaskIsRedeliveredWhenWorkerDiesBeforeStart` 就覆盖这个情况。流程是：

1. worker-1 poll 到 task。
2. worker-1 不调用 `StartTask`。
3. 等待超过 redelivery timeout。
4. worker-2 poll。
5. worker-2 应该拿到同一个 task。

这个测试说明 redelivery 不只处理 running task，也处理“已 lease 但未 start”的任务。真实系统里这类故障很常见，比如 worker poll 成功后进程崩溃、网络断开、本地 queue dispatch 失败。

## Q718. control restart probe 如何证明 metadata bootstrap 生效？

它关注的是 control 重启后的 metadata view 是否能从 shared log 恢复出可用状态，单纯进程能重新启动还不够。

集成测试里有几个关键场景：

- `TestOrdinaryTaskSurvivesControlRestartFromTaskSpecLog`：提交普通 task 后重启 control，再启动 worker，任务仍能执行成功。这说明 `TaskSubmitted` 里的 TaskSpec 能被 bootstrap 恢复。
- `TestControlRestartBootstrapsWorkflowAndModelStateFromLog`：注册模型、设置调度策略、提交 workflow，然后重启 control。重启后还能提交 LLM 请求，原 workflow 也能继续执行完成。这说明 model registry、scheduler policy、workflow state 从 log 恢复出来了。
- `TestControlRestartBootstrapsWorkerCacheFromLog`：worker cache 信息写入 log 后，control 重启仍能在 dashboard 里看到 worker 的 cached model。

这些测试比“进程没崩”更有价值。它们验证的是 metadata 不是 source of truth，control 重启后可以依赖 log 重建 view。

## Q719. logd restart probe 当前覆盖到什么程度？

当前覆盖到单机 logstore 的重启恢复和进程重启探针，还没有覆盖多副本 log 服务的高可用。

已有代码里，LogStore 单元测试会覆盖 append/read/recovery、partial tail 处理、segment/index 恢复、fsync policy 下的 reopen。`scripts/fault_injection.ps1` 还会启动 logd，停掉 logd，再用同一个 data dir 启动 logd，检查进程能重启。

Linux 实验汇总里，`logd_restart_probe` 写的是 `covered_by_logstore_recovery_and_process_logs`。这个表述比较准确。它说明本地 log 文件恢复路径被测试覆盖了，但还没有模拟这些更重的场景：

- logd 正在 append 时被 `kill -9`。
- fsync 前后崩溃。
- segment 尾部 partial record。
- index 文件损坏。
- 多个 client 重试 append。
- 多副本 leader 切换。

所以我会说：当前 logd restart probe 能证明单机持久化恢复基本可用，但不是分布式 log 服务的可用性证明。

## Q720. race test 只跑 internal/control 和 internal/worker 是否足够？

不完全足够，但这个选择是有重点的。

`internal/control` 和 `internal/worker` 是并发最密集的两个包。control 里有 queue、worker heartbeat、redelivery、workflow 调度、actor mailbox、LLM stats materialization。worker 里有 poll loop、heartbeat loop、本地 executor pool、task/LLM/actor queue、Python runner 管理。先对这两个包跑 race test，收益最大。

但只跑这两个包不能覆盖所有并发风险。比如 logstore、metadata store、object store adapter、gRPC server 边界、SDK 端并发调用，都可能有数据竞争或竞态语义问题。

我会把当前 race test 定位为“重点路径 race smoke test”。它说明 control/worker 当前测试覆盖到的并发路径没有被 Go race detector 报警，但不能证明整个项目没有并发 bug。

## Q721. 哪些模块还应该加 race test？

我会优先补这几块。

第一是 `internal/logstore`。LogStore 有全局锁、segment rollover、index rewrite、retention trim 和 stream stats。现在它有功能测试，但并发 append/read/trim 的 race test 可以再加强。

第二是 `internal/metadata`。MemoryStore 维护 task、workflow、actor、worker、model 等 map。虽然有锁和 clone 返回，但并发 create/update/list 仍然值得跑 race。

第三是 `internal/objectstore` 和 result store 相关路径。尤其是大结果写入、actor snapshot 写入、result_ref 读取，未来如果加入异步 GC 或 S3 adapter 并发访问，race test 更重要。

第四是端到端 integration test。可以用 `go test -race ./tests/integration` 跑一部分关键测试。它会慢一些，但能覆盖包之间的并发组合。

如果 CI 时间有限，我会分层：PR 上跑 control/worker race；nightly 再跑 logstore、metadata 和 integration race。

## Q722. 如何设计 crash consistency 测试而不只是 graceful restart？

graceful restart 太温和。真正要测 crash consistency，需要在写入中途强杀进程，然后检查恢复结果。

可以设计一个独立 crash harness：

1. 启动 logd，fsync policy 分别设为 `always`、`batch`、`interval`。
2. 单独起 producer 进程持续 append，并把已经收到成功响应的 idempotency key 写到外部确认文件。
3. 随机时间点 `kill -9` logd 或直接杀整个进程组。
4. 用同一个 data dir 重启 logd。
5. 读取所有 stream，检查 record CRC、seq 单调性、duplicate append 语义和 confirmed records 是否符合该 fsync policy 的承诺。

这里要注意：如果是 `batch` 或 `interval`，收到成功但未 sync 的记录可能丢失，这不一定是 bug，前提是文档明确了 durability window。`always` 下，成功返回后丢失就严重得多。

更硬的测试还可以用文件系统 fault injection，比如 dm-flakey、FUSE mock、虚拟机快照回滚，模拟断电和 partial write。这个比普通单元测试麻烦，但对 WAL 类系统很有价值。

## Q723. 如何注入磁盘写失败、fsync 慢、object store 超时？

这类故障最好通过接口注入，不要只靠 sleep 或手工杀进程。

对 LogStore，可以把底层文件操作抽象成一个小接口，测试里替换成 faulting writer。它可以在第 N 次 write 失败、在 index write 失败、在 log file sync 卡住、在 sync 返回错误。这样能精确覆盖“log 写成功但 index 写失败”“index 写成功但 sync 失败”等路径。

对 fsync 慢，可以在 logstore 或 logd 层加测试 hook，让 `Sync()` 人为 sleep。control 已经有 `last_log_append_ms` 和 `log_append_slow_ms` backpressure，慢 fsync 注入后就能验证提交是否被限流。

对 object store，可以实现一个 fault-injection store：

- `Put` 超时。
- `Put` 成功但 `Get` 短暂失败。
- `Get` 返回损坏 payload。
- `Delete` 失败。

这样能测试 workflow large result、actor snapshot、checkpoint manifest 这些路径。当前项目已经有 result store 边界，下一步把这些 failure mode 做成测试实现，会比在真实 MinIO 上随机制造故障更稳定。

## Q724. 如何让 benchmark 可重复？需要固定哪些环境变量？

要让 benchmark 可重复，先把运行环境和 workload 固定下来。

当前 Linux runner 已经支持一批环境变量：

- `LOGSERVE_EXPERIMENT_ID`
- `LOGSERVE_EXPERIMENT_DIR`
- `LOGSERVE_EXPERIMENT_DATA_DIR`
- `LOGSERVE_EXPERIMENT_LOG_ADDR`
- `LOGSERVE_EXPERIMENT_CONTROL_ADDR`
- `LOGSERVE_CHECKPOINT_SOURCE_DIR`
- `LOGSERVE_CHECKPOINT_CACHE_BYTES`
- `LOGSERVE_WORKER_CAPACITY`
- `LOGSERVE_TASK_POOL_SIZE`
- `LOGSERVE_LLM_POOL_SIZE`
- `LOGSERVE_ACTOR_POOL_SIZE`
- `LOGSERVE_BENCH_WORKFLOWS`
- `LOGSERVE_BENCH_TASKS`
- `LOGSERVE_BENCH_ACTOR_COMMANDS`
- `LOGSERVE_BENCH_LLM_REQUESTS`
- `LOGSERVE_LOGBENCH_RECORDS`
- `LOGSERVE_LOGBENCH_STREAMS`
- `LOGSERVE_LOGBENCH_PAYLOAD_BYTES`
- `LOGSERVE_LOGBENCH_SEGMENT_SIZE_BYTES`
- `LOGSERVE_LOGBENCH_POLICIES`
- `LOGSERVE_LOGBENCH_FSYNC_INTERVAL_MS`

还要固定代码版本和机器状态。`environment.txt` 里记录了 Go/Python 版本、kernel、git commit 和 git status，这很重要。最好在实验前清理旧 runtime 目录，固定 checkpoint 大小，先做 warm-up，再开始正式计时。

如果需要论文式报告，还应该每个配置跑多轮，并把原始 JSON 都保留。只保留最后一个 summary，不方便复查。

## Q725. 单机三 worker 是否能代表分布式调度？

不能代表完整分布式调度，但能验证调度器的基本决策逻辑。

单机三 worker 的好处是变量少。control、logd、worker 都在同一台机器上，网络延迟很低，部署成本也低。它适合检查：

- worker cache 状态是否上报。
- scheduler 是否按 policy 选择 worker。
- locality-aware 是否偏向缓存命中的 worker。
- redelivery 和 lease epoch 是否生效。
- dashboard 是否能显示 worker、task、model cache。

它不能覆盖真实分布式环境里的问题：

- 跨机网络抖动。
- worker 与 control 网络分区。
- 节点磁盘性能差异。
- clock skew。
- 多机 checkpoint 下载竞争。
- GPU 拓扑和显存碎片。
- 多副本 logd 的一致性。

所以单机三 worker 是“调度逻辑实验”，不是“分布式系统性能实验”。如果面试官追问，我会主动说下一步要在多机或 Kubernetes 环境里补网络和资源异构测试。

## Q726. 实验指标应该分 p50/p95/p99 还是平均值？

应该同时看，但不能只看平均值。

平均值容易被少数长尾拉高，也容易掩盖尾延迟。runtime 系统里，用户更关心“多数请求多快”和“慢请求慢到什么程度”。所以 p50、p95、p99 都有意义：

- p50 看常规请求体验。
- p95 看比较慢的一批请求。
- p99 看尾部风险。
- 平均值可以辅助判断总体成本，但不适合作为唯一指标。

LogServe 的 LLM locality ablation 里记录了 p50/p95/p99、cache hit rate、cold start rate、queue wait 和 SLO violation rate，这比只报平均值更有解释力。

样本量小时，p95/p99 会不稳定。这个时候也要报告 requests 数量。比如 6 个请求的 p99 和 6000 个请求的 p99，可信度完全不同。

## Q727. dashboard snapshot 的数据是否能用于线上监控？

可以作为监控数据源的原型，但不能直接等同于完整线上监控系统。

Dashboard snapshot 当前是一次性导出的 materialized view，适合本地实验、调试、报告和手动检查。它能看到 queue depth、task 状态、workflow step、actor owner、worker cache、model registry、log append latency 和 compactable bytes。

线上监控还需要更多东西：

- 周期性采集。
- 时间序列存储。
- 指标聚合和标签维度控制。
- 告警规则。
- 权限隔离。
- 数据量控制。
- tracing 关联。

如果接 Prometheus，可以把 snapshot 里的部分字段转成 metrics，比如 queue depth、running tasks、cache hit rate、redelivery count、log append latency、actor mailbox backlog。snapshot 本身仍然适合做“当前状态 dump”，但长期趋势最好放进 metrics 系统。

## Q728. structured log 是否足够？是否需要 metrics 和 traces？

structured log 不够。它很有用，但只能解决一部分可观测性问题。

structured log 适合记录离散事件，比如 `TaskSubmitted`、`workflow_completed`、`actor_command_applied`、`worker_poll_failed`。它能帮助排查某个 task 或 workflow 的历史。

metrics 适合看聚合趋势，比如 queue depth、task throughput、p95 latency、cache hit rate、redelivery count、log append latency、compactable bytes。没有 metrics，很难判断系统是否正在变慢。

traces 适合看单个请求穿过多个组件的路径。比如一个 workflow 从 SDK 到 control，再到 worker，再到 Python executor，再回到 CompleteTask。只靠日志也能查，但很费力。

所以更完整的方案是三者一起用：

- logs 解释发生了什么。
- metrics 说明系统整体是否健康。
- traces 还原一次请求为什么慢。

当前项目已经有 structured logs 和 dashboard snapshot，下一步应该补 metrics endpoint 和 trace context propagation。

## Q729. 如何给每个 workflow/task/actor call 生成 trace_id？

我会从 SDK 入口生成 trace id，然后沿请求链路传播。

具体做法是：

1. SDK 在 `submit()`、`submit_workflow()`、`call_actor()`、`submit_llm()` 时生成或接收一个 `trace_id`。
2. 把 `trace_id` 放进 gRPC metadata，也放进 TaskSpec 或事件 payload。
3. control 在 append log、更新 metadata、调度 task 时保留这个字段。
4. worker poll 到 task 后，把 `trace_id` 传给 executor、LLM adapter 和 completion 请求。
5. 所有 structured log 都带 `trace_id`，同时带 `span_id` 和 `parent_span_id`。

对于 workflow，还需要 `workflow_id`、`step_id` 和 `task_id`。对于 actor call，需要 `actor_id`、`actor_call_id` 和 `command_seq`。这些字段和 trace id 互补，负责业务定位。

如果接 OpenTelemetry，可以把 SDK submit、control scheduling、worker execution、LLM adapter call 都做成 span。这样看一次慢请求时，不需要在多份日志里手工拼接。

## Q730. 如何将 queue wait、executor time、model load time 拆成 latency breakdown？

要在每个阶段记录时间戳，而不是只记录总耗时。

task 路径可以拆成：

- `submitted_at_ms`：SDK 或 control 接收请求。
- `queued_at_ms`：进入 control queue。
- `leased_at_ms`：被 PollTask 分配给 worker。
- `worker_enqueued_at_ms`：进入 worker 本地 executor queue。
- `started_at_ms`：executor 真正开始执行。
- `finished_at_ms`：用户函数或 LLM 调用结束。
- `completed_at_ms`：CompleteTask 写入完成状态。

这样可以算：

- control queue wait = `leased_at_ms - queued_at_ms`
- local queue wait = `started_at_ms - worker_enqueued_at_ms`
- executor time = `finished_at_ms - started_at_ms`
- completion overhead = `completed_at_ms - finished_at_ms`

LLM 还要拆：

- checkpoint fetch time
- model load time
- first token latency
- decode 或 total generation time

当前 ReplayLLM 已经有 `checkpoint_fetch_ms`、`model_load_ms`、`first_token_ms`、`total_latency_ms`。后续可以把这些字段统一接进 dashboard 和 traces，形成每个请求的 breakdown。

## Q731. 如何识别调度器选择错误导致的冷启动？

要记录调度决策原因，并和执行结果对比。

调度时可以为每个候选 worker 记录一份 score：

- worker id
- running tasks
- capacity
- queue penalty
- whether cached
- predicted latency
- cold start penalty
- eviction penalty
- final score

选中 worker 后，把这份 decision 写到 structured log 或 scheduler decision stream。LLM 完成后，再从 `LLMCompleted` 里看实际 `cache_hit`、`checkpoint_fetch_ms`、`model_load_ms` 和 total latency。

如果某次请求被分到未缓存 worker，实际出现 cold start，而当时有另一个可用 worker 已经缓存目标模型，就可以判定为调度器可能选错。还要看 cached worker 当时是否满载。如果 cached worker queue 很长，选择 cold worker 可能是合理 tradeoff。

最有用的 dashboard 视图是：一次 LLM 请求展示候选 worker score、最终选择、实际 cache hit/miss 和 latency breakdown。这样调度 bug 会很直观。

## Q732. 如何可视化 actor mailbox backlog？

需要先把 backlog 变成可观测字段。

control plane 里可以为每个 actor 维护：

- submitted command count
- applied command count
- pending command count
- oldest pending command age
- current owner worker
- epoch
- last applied command seq
- failed command count

`pending command count = submitted command count - applied command count`。这就是 mailbox backlog 的核心指标。

Dashboard 上可以按 actor 展示一张表：actor id、owner、epoch、pending commands、oldest pending age、command throughput、last error。再加一个排序，默认按 pending commands 或 oldest age 排。这样热点 actor 会直接浮到前面。

对于单个 actor，可以画 command_seq 时间线：submitted、started、applied、failed。这样能看出是 owner worker 慢、executor 慢、还是某个 command 卡住了。

## Q733. 如何可视化 shared log segment 和 compactable bytes？

可以分两层看。

第一层是 logstore 物理层：

- segment id
- segment size
- record count
- active 或 closed
- index 是否存在
- last seq range

这能帮助判断 segment rolling 是否正常，磁盘增长是不是过快。

第二层是 stream retention 层：

- stream id
- first seq
- last seq
- trim before seq
- total records
- compactable records
- compactable bytes

Dashboard 可以把这些做成 stream 表格，按 compactable bytes 排序。actor stream 如果 snapshot 生效，compactable bytes 应该逐渐出现。workflow 或 task stream 如果还没有 retention 策略，compactable bytes 可能为 0。

要提醒一点：segment 是物理文件，stream 是逻辑日志。一个 segment 里可能混着多个 stream 的记录。physical compaction 不能只看某个 stream compactable，就直接删整个 segment；要确认 segment 内所有记录都不再被任何 stream replay 需要。

## Q734. 如何设置 SLO violation 计算规则？

先定义用户感知的目标，再把它转成明确公式。

比如 LLM request 可以设置：

```text
SLO: 95% requests total latency <= 250ms
violation: request_total_latency_ms > 250
```

benchmark 里当前做法是给每个请求计算 elapsed time，如果超过 `slo_ms=250`，就计为一次 violation，最后输出 `slo_violation_rate`。

线上场景要更细：

- 区分冷启动请求和热请求。
- 区分模型大小和 prompt 长度。
- 区分成功请求和失败请求。
- 明确 timeout 是否算 violation。
- 明确重试后的最终成功是否仍记录原始慢请求。

Workflow 的 SLO 可以按端到端完成时间算，也可以按关键 step 算。Actor 的 SLO 可以按 command submit 到 applied 的时间算。Shared log 的 SLO 可以按 append latency 算。

不要只设一个全局 SLO。不同 runtime 对延迟的敏感点不一样。

## Q735. 如何向面试官解释实验结果的可信度和局限？

我会把话说清楚，不把实验包装得太满。

可信的部分是：这些实验跑的是仓库里的真实代码路径，不是 README 里的设计图。Linux runner 会启动 logd、control、3 个 worker，跑 Go tests、`go vet`、race test、Python unittest、compileall、logstore benchmark、fault injection、runtime benchmark、checkpoint cache probe 和 dashboard snapshot。每一步都有 exit code、耗时和日志文件，最后还有 `summary.json` 和 `summary.md`。

实验能支持这些结论：

- shared log append/read/recovery 路径可运行。
- workflow 能按 DAG step 执行并 replay。
- worker 丢失后 task 可以 redelivery。
- actor snapshot 能减少 replay commands。
- locality-aware scheduling 能在 mock 环境里提高 cache hit rate。
- checkpoint cache 确实写入 worker-local artifact。
- dashboard 能导出 materialized runtime state。

局限也很明确：

- 单机实验，不代表多机网络环境。
- mock LLM，不代表真实 GPU serving 性能。
- 默认样本量小，p95/p99 主要是机制验证。
- logd 是单机，不是多副本共识日志。
- checkpoint 文件很小，不代表几十 GB 模型。
- fault injection 还没覆盖所有 crash consistency 和磁盘故障。

这种说法更可信。项目已经有工程证据，但还没到生产性能报告的强度。下一步要补多轮大样本、真实 GPU、多机部署、crash consistency、对象存储故障和长期运行实验。
