# LogServe 优化技术说明

这份文档只整理当前项目里已经落地或已经进入明确路线图的优化。阅读时先看“已落地”，再看“后续路线”。更长的设计备忘录在 `docs/plan.md`，但那份文件包含历史计划，当前状态以本文和 `docs/report.md` 为准。

## 优化原则

LogServe 的优化不是堆名词，而是围绕几个真实瓶颈做工程拆解：

1. shared log 的写入、读取、恢复成本。
2. control plane 的队列扫描、worker poll、redelivery 和 LLM placement。
3. metadata view 的锁竞争和 PostgreSQL 写放大。
4. workflow/actor replay 的历史日志读取量。
5. worker 执行路径里的序列化、排队和模型 checkpoint cache。

不优先做 DPDK、NUMA、CPU affinity、Direct I/O、io_uring 这类更底层技术。它们不是不能做，而是当前 profile 和验收场景还没有证明它们是主瓶颈。

## 已落地优化

### scheduler v2

旧版控制面用一个全局队列保存所有 task。worker poll 时从头扫描，遇到 actor、LLM、target worker 等任务还要继续做额外判断。backlog 深时，这个模型容易把调度成本放大。

scheduler v2 把任务按类型拆开：

- 普通任务走 general queue。
- 指定 worker 的任务走 target-worker queue。
- actor command 走 `actorPending[actor_id]`。
- LLM 请求使用模型 placement index。
- redelivery 使用 lease 信息和调度状态，减少重复全表扫描。

这样普通 task 不会被大量 actor/LLM 任务挡住，actor 也不会和普通队列混在一起。最新 Compose 实验中，mixed backlog benchmark 在 100,000 backlog、100 workers 下为约 `4078 ns/op`、`2468 B/op`、`5 allocs/op`，没有随 backlog 深度线性恶化。

### worker batch poll 和 long-poll

worker 不再只靠固定 tick 拉一个任务。现在 `PollTask` 支持：

```text
max_tasks
wait_timeout_ms
```

worker 会按本地 executor pool 的空闲容量批量拉取任务，空闲时 long-poll 等待新任务通知。完成结果也通过 `CompleteTasks` 批量提交。这个优化降低了空轮询和 RPC 次数，也让低负载任务不必等下一次 poll tick。

server-streaming 暂时没有开放为正式协议。原因很简单：batch + long-poll 已经解决主要问题；streaming 还需要定义流控、断线重连、lease 回收和 backpressure，复杂度更高。

### metadata store v2

metadata 的主要成本来自锁粒度和 clone。旧版 `MemoryStore` 用一把大锁管理 task、worker、workflow、actor 和 model。v2 把热点路径拆开，增加索引，并改善 heartbeat 路径。

最新 benchmark 中：

| 指标 | Legacy | V2 | 结论 |
|---|---:|---:|---|
| `GetTask` | 66.30 ns/op | 28.44 ns/op | 读取更快 |
| `Heartbeat` | 1526 ns/op | 169.1 ns/op | 心跳路径明显更轻 |
| heartbeat p99 under complete | 5,382,126 ns | 17,242 ns | 并发 complete 下抖动大幅下降 |
| `LeaseComplete` | 1715 ns/op | 3767 ns/op | 写路径仍有优化空间 |
| `UpdateWorkflow` | 12664 ns/op | 20912 ns/op | workflow 写路径仍有成本 |

这组结果说明 v2 不是所有路径都更快。文档和面试中应该说：它显著改善 heartbeat 和读取热点，但 LeaseComplete、ActiveWorkers、UpdateWorkflow 仍是后续优化点。

### PostgreSQL async materializer

PostgreSQL 是 dashboard 和查询用的 materialized view，不是 source of truth。source of truth 是 shared log。

同步模式下，控制面每次状态变化都会同步 upsert PostgreSQL。async 模式改为：

```text
write shared log
update memory view
enqueue metadata delta
background batch flush to PostgreSQL
```

最新 Ubuntu 顶层验收中的对比：

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput | 5.0 tps | 5.0 tps | 1.0 |
| Task submit p99 | 209 ms | 207 ms | 0.9904 |
| PostgreSQL tx/s | 72.382 | 1.304 | 0.018 |
| PostgreSQL row writes/s | 100.519 | 16.57 | 0.1648 |

正确结论是：数据库写入压力显著下降，任务路径没有退化。不要说吞吐显著提升，因为这次严格吞吐提升为 false。

### metadata checkpoint

没有 checkpoint 时，control 重启需要从历史日志重新 materialize metadata。metadata checkpoint 把 task、workflow、actor、LLM stats 和各 stream 的 `last_seq` 写到 `system:checkpoints`。重启时先加载 checkpoint，再只读每个 stream 的 tail。

最新验收 workload：

| Item | Count |
|---|---:|
| Tasks | 120 |
| Workflows | 12 |
| Actors | 12 |
| LLM streams | 40 |
| Tail events | 68 |

结果：

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration | 6.463 ms | 5.506 ms | 0.8519 |

更稳的结论是“历史 records 读取量下降”，不是“消除 stream 访问”。因为 checkpoint 模式仍需要检查 stream tail。

### actor snapshot、logical trim 和 physical compaction

actor snapshot 解决 replay work，logical trim 标记 snapshot 前的日志可以跳过，physical compaction 再处理磁盘空间。

当前链路是：

```text
ActorCommandApplied
ActorSnapshotCreated -> snapshot_ref
Trim(actor stream before snapshot)
CompactabilityStats
Compact(delete fully trimmed segments / copy live records)
```

最新 Compose 实验中，同样 20 次 actor command 后：

| 指标 | 结果 |
|---|---:|
| full replay commands | 21 |
| no-snapshot replay commands | 21 |
| snapshot replay commands | 1 |
| trimmed replay commands | 1 |
| compactable actor-log records | 45 |
| compactable actor-log bytes | 18,382 |

顶层验收同时跑过 physical compaction focused tests 和 logstore race tests。可以说 delete/copy/crash window 下没有破坏 replay 语义；不要把这扩大成长期生产存储回收能力已经验证完毕。

### logstore checksum、raw read 和 mmap read

logstore 的优化集中在读写和恢复路径：

- 新记录默认 CRC32C，旧记录按 IEEE CRC32 兼容读取。
- header 记录 checksum type，支持 `IEEE`、`CRC32C`、`XXH3`、`None`。
- 大 payload 按 64 KiB chunk 做 checksum，避免一次性大块校验。
- `ReadRawEach` 让 replay reducer 直接消费 raw payload，减少对象构造。
- `ReadLogStream` 支持 gRPC streaming read。
- Linux/macOS 上 sealed segment 可以启用 mmap read，active segment 仍走 `ReadAt`。
- compaction 会释放 mmap mapping，避免删除文件时还持有旧映射。

最新 logstore benchmark：

| fsync policy | Append records/s | Read records/s | Recover ms |
|---|---:|---:|---:|
| always | 1,760.664 | 656,928.533 | 36 |
| batch | 293,717.448 | 906,235.115 | 36 |
| interval | 296,456.268 | 999,045.612 | 31 |

结论很直接：强同步 `always` 最慢；放宽 fsync 的 `batch` 和 `interval` 写入吞吐高很多。恢复时间在这次单机 workload 下仍是几十毫秒量级。

### workflow RuntimeDAG

workflow 内部不再每次扫描所有 step 来找 ready step，而是维护：

```text
steps in topological order
stepID -> index
outgoing edges
remaining dependency count
ready queue
```

step 成功后只更新下游边。retry、timeout、replay 和 checkpoint tail replay 都会重建 runtime index。外部 API 仍然保留 `step_id`，不会影响 SDK 或 dashboard。

### msgpack executor 和内部事件编码

Python executor 默认使用 length-prefixed msgpack frame，减少 JSON line 协议的序列化成本。可以用 `LOGSERVE_EXECUTOR_PROTOCOL=json` 回退。

控制面内部部分事件使用 msgpack，并带 `LSE\x01` magic。旧 JSON payload 仍可 fallback 读取。CLI 和 dashboard 仍保持 JSON 表示，避免把内部优化暴露成用户负担。

### benchmark、pprof 和回归门禁

项目已经有一套脚本把优化和证据连起来：

```bash
bash scripts/benchmark_micro.sh
bash scripts/run_experiment.sh
bash scripts/ubuntu_project_acceptance.sh
bash scripts/ubuntu_checkpoint_acceptance.sh
bash scripts/ubuntu_postgres_async_acceptance.sh
```

建议每个后续优化都保留五类证据：

1. 正确性测试。
2. microbenchmark 前后对比。
3. macro benchmark 或验收脚本。
4. pprof 或 dashboard 证据。
5. feature flag 或回滚方式。

## 后续优化路线

### P0：继续压低 shared log 热路径成本

后续最值得做的是 AppendBatch + group commit。目标不是堆复杂 I/O 技术，而是减少每条 append 独立 write/fsync 的成本。

建议拆成：

1. 抽出 append 编码和提交逻辑。
2. 加 writer goroutine。
3. 先保持单条 flush，保证行为不变。
4. 加 batch 聚合和 group commit。
5. 增加 batch crash tests。
6. 再考虑 batch proto。

### P0：完善 read/index 路径

下一步应继续减少大 stream 读取成本：

- `fromSeq` 读取用二分定位。
- segment fd cache 降低 open/close churn。
- binary index 降低 index 文件体积和恢复解析成本。
- `ReadLogStream` 逐步用于更多 bootstrap/replay 路径。

### P1：worker/cache/object store

worker 侧后续重点：

- Python function registry，减少重复 `function_source`。
- Python compile cache，减少同一函数反复编译。
- checkpoint cache per-model singleflight，避免多个模型冷启动互相阻塞。
- O(1) LRU，替代扫描式淘汰。
- object store streaming Put/Get，降低大 result/snapshot 的内存峰值。

### P1：更强实验

当前实验已经能证明单机机制，但还不能证明生产负载。下一阶段更有价值的是：

- 多节点部署。
- 真实 vLLM/GPU 请求。
- 更大 checkpoint 的 cold-start 曲线。
- 更长时间运行后的 compaction 空间回收曲线。
- metadata v2 写路径优化前后的 mutex/heap profile。

## 不优先做的技术

| 技术 | 当前不优先的原因 |
|---|---|
| DPDK / kernel bypass | LogServe 当前瓶颈更可能在 log I/O、metadata、调度和 executor，不是 L2/L3 包处理。 |
| CPU affinity | 没有 profile 证明 goroutine migration 是瓶颈，强绑核可能降低调度弹性。 |
| NUMA | 当前单机 mock LLM 和小 checkpoint 场景不适合优先投入。 |
| huge page | 更可能对真实 vLLM/GPU serving 有意义，不是 control/logstore 的首要优化。 |
| Direct I/O | 会增加对齐和 buffer 管理复杂度，还可能失去 page cache 对 replay 的帮助。 |
| io_uring | 可以作为 Linux-only 实验，但应在 batch write、fd cache、mmap read 之后再评估。 |
| lock-free queue | 当前问题是队列模型，不是单纯 push/pop 成本。先做 typed/indexed scheduler 更合理。 |
