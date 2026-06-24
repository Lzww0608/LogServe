# LogServe 优化空间与执行计划：数据结构与基础架构方向

---

## 1. 总体判断

LogServe 当前已经是一个完整的 shared-log AI runtime，核心机制包括 shared log、log-first control plane、workflow DAG、actor、LLM serving、checkpoint cache、benchmark 和 fault injection。项目已经具备面试展示价值。下一阶段如果要继续提升，最值得投入的不是 API 表层，而是下面几个基础结构：

1. `logstore` 的 append/read/recovery 路径。
2. `control` 的队列、调度、redelivery 和 workflow step scheduling。
3. `metadata` 的锁粒度、索引结构、clone 开销和 PostgreSQL materialized view 写入方式。
4. `worker` 的 polling、executor pool、Python code 分发和 checkpoint cache。
5. result/object store 的 streaming、原子写、S3 请求复用和大对象路径。
6. replay/bootstrap/compaction 的增量化。

这些方向比直接引入 DPDK、CPU affinity、NUMA、huge page 更有价值。后者只有在项目进入多机、高吞吐、真实 GPU/vLLM、真实大 checkpoint 和生产压测后才值得做。

---

## 2. 优先级矩阵

| 优先级 | 优化方向 | 预期收益 | 复杂度 | 风险 | 建议结论 |
|---|---|---:|---:|---:|---|
| P0 | logstore AppendBatch + group commit | 高 | 中 | 中 | 立即做 |
| P0 | control 队列从线性扫描改成 typed/indexed scheduler | 高 | 中高 | 中 | 立即做 |
| P0 | ReadLog 使用二分索引、segment fd cache、streaming read | 中高 | 中 | 中 | 立即做 |
| P0 | metadata sharding + 状态索引 + 减少 clone | 中高 | 中 | 中 | 立即做 |
| P1 | recovery/bootstrap 增量 checkpoint | 高 | 高 | 中高 | 分阶段做 |
| P1 | physical compaction | 高 | 高 | 高 | 做，但要隔离实现 |
| P1 | PostgreSQL async materializer / batch upsert | 中高 | 中 | 中 | 做 |
| P1 | Python function/code registry，减少重复 function_source | 中高 | 中 | 低中 | 做 |
| P1 | checkpoint cache per-model lock + O(1) LRU | 中 | 中 | 低 | 做 |
| P1 | object store streaming Put/Get + local atomic write | 中 | 中 | 低中 | 做 |
| P2 | CRC32C Castagnoli + 硬件加速验证 | 中 | 低中 | 低 | 做为 logstore 子任务 |
| P2 | mmap read path 实验 | 中 | 中高 | 中 | 实验，不先替换主路径 |
| P2 | sync.Pool / buffer pool | 中 | 低 | 中 | 在 profile 证明确有 GC 压力后做 |
| P2 | gRPC streaming / batch RPC | 中 | 中 | 中 | 与 AppendBatch/ReadLog 一起做 |
| P3 | Direct I/O | 不确定 | 高 | 高 | 暂不建议 |
| P3 | io_uring | 不确定 | 高 | 高 | 作为 Linux 专项实验 |
| P3 | lock-free queue | 不确定 | 高 | 高 | 先做 indexed scheduler，不急 |
| P3 | CPU affinity / NUMA / huge page | 低或场景依赖 | 中高 | 中 | 真实多机/GPU 后再做 |
| P3 | DPDK / kernel bypass | 极低 | 极高 | 极高 | 当前不建议 |

---

## 3. 审查到的关键现状

### 3.1 Shared log

当前 `internal/logstore/store.go` 里 `Store` 维护单个 `sync.Mutex`、active log/index file、`nextSeq map`、`index map[string][]indexEntry`、`idempotency map` 和 `trimBefore map`。`Append` 在持有同一把锁时完成幂等检查、序列号分配、record 编码、文件写入、index 写入、fsync、内存 index 更新。这个路径正确性清晰，但当 append QPS 上去后，锁等待、系统调用次数、fsync 次数和 JSON index 开销会成为主要瓶颈。

`Read` 会在锁内从 `s.index[streamID]` 线性扫描，挑出 `Seq >= fromSeq` 的 entry，再解锁并打开 segment 文件读 payload。这个实现简单，但当单个 stream entry 数量增长时，`fromSeq` 越靠后，线性扫描越浪费。

`recover` 会扫描所有 `.log` segment，重建内存 index 和 idempotency map，并重写 index 文件。当前数据规模小，恢复时间可控；一旦日志规模到 GB 级，启动时间会变成可见问题。

### 3.2 Control plane

`Service` 中任务队列是 `[]string`，`PollTask` 每次 worker poll 都拿 `queueMu`，从头扫描队列，逐个查 spec、查 metadata、检查 actor mailbox、检查 LLM scheduler，然后用 `append(s.queue[:i], s.queue[i+1:]...)` 删除命中的 task。这个数据结构在小规模下可靠，但本质是 O(queue_depth * polling_workers)，并且会在持有队列锁时做多次 metadata 调用和 scheduler 计算。

LLM scheduler 当前会在每次候选判断时扫描 active workers，`LOCALITY_AWARE` 和 `PREDICTED_LATENCY` 都是 O(number_of_workers)。这个复杂度单独看可以接受，但嵌在 `PollTask` 队列扫描内，整体可能退化为 O(queue_depth * workers)。

redelivery 目前基于 metadata 里的 `RequeueExpiredRunningTasks(maxAge)`，内存实现会扫描全部 tasks。它如果在每次 PollTask 前执行，就会在 worker 数增多时产生重复全表扫描。

### 3.3 Metadata

`MemoryStore` 是全局 `sync.RWMutex`，所有 task、worker、workflow、actor、model 都共享同一把锁。读方法会 clone 对象，写方法会整对象复制回 map。这个设计对正确性很友好，但在高频路径上存在三个成本：锁竞争、全局锁域过大、频繁 clone/拷贝。

`PostgresStore` 是 memory store + 同步 persist 的双层结构。每次 `CreateTask`、`LeaseTask`、`CompleteTask`、`Heartbeat`、`UpdateWorkflow`、`UpdateActor` 都先改 memory，再用 `context.Background()` 同步执行 upsert。由于 shared log 是 source of truth，PostgreSQL 更适合做异步 materialized view，而不是每个内存状态变更的同步依赖。

### 3.4 Worker

Worker 使用本地 executor pool，分别有 task、LLM、actor queue。普通任务和 actor 任务使用 Python runner；LLM 任务走 mock/vLLM 路径。worker 通过固定 `PollInterval` 做心跳和拉取任务。这个模型便于调试，但会引入 poll latency、空轮询、无法 batch 分配等问题。

Python executor 每次根据传入的 `function_source` 编译执行；SDK 端会把函数源码或整个 module source 带到 TaskSpec。对于 workflow 中多个 step 或多次提交同一函数，这会带来重复日志 payload、重复 gRPC payload 和重复 Python compile 成本。

### 3.5 Object store 和 checkpoint cache

LocalStore 用 SHA256 内容寻址并直接 `os.WriteFile`。它没有 temp file + rename + directory fsync 的原子提交流程，也没有 streaming Put/Get。S3Store 每次 Put 可选择 ensure bucket，并且对 data 做整块 SHA256 和整块 HTTP request；对大对象不支持 streaming/multipart。

worker 的 model cache 有全局 `checkpointMu`，这会把所有 checkpoint miss 串行化。对多个模型并发冷启动，最理想的数据结构应该是 per-model lock + O(1) LRU，而不是全局锁和扫描。

---

## 4. P0 优化一：logstore AppendBatch + group commit

### 当前瓶颈

`Append` 每条 record 单独 encode、单独写 log、单独写 index，并按 fsync policy 决定是否 sync。`always` 策略在 benchmark 中明显慢于 batch/interval。这个结果说明同步写盘是当前最明确、最可解释的瓶颈之一。

### 优化目标

把 logstore 从“每个 gRPC AppendLog 对应一次完整写路径”升级为“单 writer goroutine + 批量落盘 + group commit”。目标是保留 per-stream seq、idempotency 和 crash recovery 语义，同时降低 syscall、锁持有时间和 fsync 次数。

### 核心设计

新增内部单 writer：

```go
type appendRequest struct {
    req  AppendRequest
    done chan appendResult
}

type writerLoop struct {
    in chan appendRequest
    maxBatchRecords int
    maxBatchBytes   int
    maxDelay        time.Duration
}
```

外部 `Append` 不再直接持有 `Store.mu` 做文件 I/O，而是把请求放入 writer channel。writer loop 聚合请求，到达以下任一条件就 flush：

1. batch record 数达到阈值，例如 64 或 256。
2. batch byte 数达到阈值，例如 1 MB。
3. oldest request 等待超过阈值，例如 1 ms 或 5 ms。
4. fsync policy 为 `always` 且需要同步返回 durability。

批处理过程：

1. 在内存中完成 idempotency 检查和 seq 分配。
2. 用连续 buffer 编码多个 records。
3. 一次或少数几次 `Write` 写入 `.log`。
4. 用二进制 index batch 写入 `.index`。
5. 根据 group commit policy 决定一次 `Sync`。
6. 批量更新内存 index、nextSeq 和 idempotency。
7. 唤醒所有等待 append 结果的请求。

### API 变化

建议在 proto 里新增：

```proto
message AppendLogBatchRequest {
  repeated AppendLogRequest records = 1;
}

message AppendLogBatchResponse {
  repeated AppendLogResponse records = 1;
}
```

控制面在 workflow scheduling、worker terminal event、LLM event 这类路径可以批量写事件。logstore 也保留单条 `AppendLog`，内部转成 batch size = 1。

### 执行步骤

1. 新增 microbenchmark：单 stream append、多 stream append、payload 256B/1KB/16KB、always/batch/interval。
2. 抽出 `appendLocked` 的纯逻辑，拆成 `prepareRecord`、`encodeRecord`、`commitIndex`。
3. 引入 writer goroutine 和 request channel，但先保持单条 flush，确保测试不变。
4. 加入 batch 聚合逻辑。
5. 新增 batch proto 和 client/server 方法。
6. 把 control 中连续 append 的场景改成 batch，例如 task started + step started 可以作为同一批提交，LLM event 也可批量提交。
7. 增加 crash test：batch 写一半、index 写一半、fsync 前后崩溃。
8. 对比 benchmark：append p50/p95/p99、records/s、fsync/s、syscall count、CPU profile。

### 验收指标

1. `always` 下吞吐至少有明显提升，核心目标不是超过 interval，而是降低单条 fsync 带来的尾延迟。
2. `batch/interval` 下 CPU 使用率和 syscall 次数下降。
3. 原有 recovery truncation、idempotent append、workflow/actor replay 测试全部通过。
4. 新增 batch crash test 通过。

### 风险

1. group commit 会改变延迟分布，需要明确 max delay。
2. batch 内某条请求 idempotency duplicate 时，响应顺序要保持和请求顺序一致。
3. 如果同一 stream 的多个 records 在同一 batch 中，seq 分配必须严格递增。

---

## 5. P0 优化二：二进制 index + ReadLog 二分查找

### 当前瓶颈

index 当前是内存中的 `map[string][]indexEntry`，每个 `Read` 根据 `fromSeq` 线性扫描 entries。index 文件目前用 JSON line 写入，这对 debug 友好，但写入、恢复、解析、存储密度都不理想。

### 优化目标

将 index 改为二进制 fixed-width 格式，并且在读路径用 `sort.Search` 按 seq 二分定位。目标是减少 CPU、内存和启动恢复开销。

### 数据结构建议

当前：

```go
type indexEntry struct {
    StreamID  string
    Seq       uint64
    SegmentID uint64
    Offset    int64
    Length    int64
}
```

建议分离 stream metadata 与 entry：

```go
type streamState struct {
    streamID string
    nextSeq uint64
    trimBefore uint64
    entries []streamIndexEntry
}

type streamIndexEntry struct {
    seq uint64
    segmentID uint32
    offset uint64
    length uint32
}
```

把 `StreamID` 从每条 entry 中移出，减少重复字符串。segmentID 若短期不会超过 2^32，可用 `uint32`，否则保留 `uint64`。length 通常小于 4GB，用 `uint32` 足够。

### 读路径优化

把：

```go
for _, entry := range entries {
    if entry.Seq < fromSeq { continue }
    selected = append(selected, entry)
}
```

改为：

```go
i := sort.Search(len(entries), func(i int) bool {
    return entries[i].seq >= fromSeq
})
end := min(i+limit, len(entries))
selected := entries[i:end]
```

注意：不能直接返回底层 slice 给外部使用，防止并发写 append 后底层数组变化。可以复制 selected header 或复制 entry 范围；entry 是小结构体，复制成本可控。

### index 文件格式

推荐 binary index record：

```text
magic u32
version u16
stream_id_len u16
seq u64
segment_id u64
offset u64
length u32
stream_id bytes
crc32c u32
```

或者更进一步：每个 segment index file 先写 stream dictionary，再写 fixed entry：

```text
SegmentIndexHeader
StreamDictionary[]
Entry{stream_id_id, seq, offset, length}[]
```

第二种更复杂，但 cache locality 更好。

### 执行步骤

1. 先只改内存读路径为二分查找，保持现有 JSON index 文件。
2. 加基准：单 stream 1e5/1e6 entries，fromSeq 在头部、中间、尾部的 read latency。
3. 再引入 binary index writer 和 reader。
4. 保留 JSON index 兼容读取一个版本，或者启动时检测 magic。
5. 更新 recovery：优先读 binary index；当 index 缺失或损坏时回退扫描 log。
6. 加入 index corruption test。

### 验收指标

1. 大 stream `ReadLog(fromSeq near tail)` 显著快于线性扫描。
2. index 文件体积下降。
3. recovery 不再总是重写 index，启动时间随日志规模下降。

---

## 6. P0 优化三：segment fd cache + streaming ReadLog

### 当前瓶颈

`Read` 每次读取 selected entries 时，按 segment 打开文件。读取密集时反复 `os.Open` / `Close` 会增加 syscall 和文件描述符 churn。

### 优化目标

引入 segment reader cache，减少重复打开文件；同时在 gRPC 层增加 streaming read，避免大批量日志一次性 materialize 成 `repeated LogRecord`。

### 设计建议

1. `Store` 中维护一个 LRU fd cache：`map[segmentID]*segmentReader`。
2. segmentReader 包含 `*os.File`、refcount 或 mutex、lastUsed。
3. active segment 可以复用已有 logFile，但要谨慎处理读写 offset，使用 `ReadAt` 而不是依赖 file offset。
4. 提供配置：最大缓存 segment 数，例如 64 或 256。
5. 超过上限时关闭最久未使用的只读 fd。

### streaming ReadLog API

新增：

```proto
rpc ReadLogStream(ReadLogRequest) returns (stream LogRecord);
```

适用场景：workflow replay、actor replay、control bootstrap、LLM replay。这样可以边读边 replay，避免把大量 `LogRecord` 全部加载进内存。

### 执行步骤

1. 在 logstore 内实现 fd cache，默认开启。
2. 对 ReadLog benchmark 增加跨 segment 读取场景。
3. 新增 ReadLogStream proto 和服务端实现。
4. 改造 `readAllLog` 的内部使用，在 bootstrap/replay 路径先支持 streaming。
5. 保留旧 API 兼容 SDK/CLI。

### 验收指标

1. ReadLog p99 下降，尤其是跨多个 segment 的读取。
2. bootstrap/replay 的峰值内存下降。
3. fd 泄漏测试通过。

---

## 7. P0 优化四：control 队列从线性扫描改为 indexed scheduler

### 当前瓶颈

`PollTask` 对 `queue []string` 做线性扫描，并在扫描过程中执行 spec lookup、metadata lookup、actor mailbox 判断、LLM worker preference 判断。对于 backlog 较大、worker 较多、LLM/actor 任务混合的场景，这个结构会成为控制面主要 CPU 热点。

### 优化目标

把单队列改成 typed/indexed scheduler，使普通 task、target-worker task、actor task、LLM task 可以走不同队列和索引，降低每次 poll 的扫描范围。

### 数据结构建议

```go
type Scheduler struct {
    mu sync.Mutex

    readyGeneral deque[string]
    byTargetWorker map[string]deque[string]
    actorPending map[string]deque[string]
    llmByModel map[modelKey]deque[string]

    taskMeta map[string]SchedMeta
    runningDeadlineHeap taskDeadlineHeap
}

type SchedMeta struct {
    taskID string
    taskType TaskKind
    targetWorker string
    actorID string
    commandSeq uint64
    modelName string
    modelVersion string
    createdAtMs int64
    leaseEpoch uint64
}
```

worker poll 时：

1. 先看 `byTargetWorker[workerID]`。
2. 再看 worker 是 actor owner 的 actor queue，且 commandSeq ready。
3. LLM worker 根据 cached model 集合，查 `llmByModel[modelKey]`。
4. 最后看 general queue。

这比扫描所有 queued task 更稳定。

### LLM 调度索引

当前 `preferredLLMWorker` 每次扫描 active workers。可以在 heartbeat 和 register 时维护：

```go
type modelPlacement struct {
    cachedWorkers map[string]struct{}
    coldWorkers map[string]struct{}
}
```

`LOCALITY_AWARE` 不必每次从所有 workers 中找 cached worker，而是从 `cachedWorkers[modelKey]` 和 worker capacity view 里选。

### actor 调度索引

actor 的 `command_seq` gating 可以继续保留，但 actorPending 应按 actorID 分队列。owner worker poll 时只需要检查自己拥有的 actor IDs，而不是扫整个全局 queue。

### redelivery 优化

把 redelivery 从“每次 poll 前扫描所有 running tasks”改成 deadline heap 或 timing wheel：

```go
type runningLease struct {
    taskID string
    deadlineMs int64
    leaseEpoch uint64
}
```

只在 poll 或独立 goroutine 中检查堆顶是否过期，不再扫全表。

### 执行步骤

1. 先把当前 `queue []string` 封装成 `Scheduler` 接口，不改变行为。
2. 新增 scheduler 单元测试，覆盖 FIFO、target worker、actor command_seq、LLM locality、redelivery。
3. 引入 typed queues，但保留单队列 fallback 开关：`LOGSERVE_SCHEDULER_V2=1`。
4. 将 `enqueueTask` 写入 scheduler，而不是直接 append queue。
5. `PollTask` 改成调用 `scheduler.Assign(workerSnapshot)`。
6. redelivery 从 metadata full scan 改成 scheduler deadline heap。
7. 压测 queue_depth = 1k/10k/100k，worker = 1/10/100。

### 验收指标

1. PollTask p99 随 queue depth 增长更慢。
2. queue lock hold time 显著下降。
3. scheduler 语义测试覆盖：stale completion、actor ordering、LLM locality、workflow retry。
4. 在 backlog 混合任务下，普通 task 不被大量 actor/LLM task 阻塞。

---

## 8. P0 优化五：metadata sharding、状态索引和减少 clone

### 当前瓶颈

`MemoryStore` 用一把 `sync.RWMutex` 管理所有任务、worker、workflow、actor 和 model。很多读取都会 clone 结构体；workflow/actor 状态可能包含较大的 JSON byte slice，clone 会放大内存分配和 GC 压力。

### 优化目标

把全局锁拆成多个 shard 和 domain lock，减少热点锁争用；同时增加状态索引，避免 List/Scan；减少不必要的深拷贝。

### 数据结构建议

```go
type MemoryStoreV2 struct {
    tasks taskStore
    workers workerStore
    workflows workflowStore
    actors actorStore
    models modelStore
}

type taskStore struct {
    shards [64]taskShard
    byStatus statusIndex
    byWorker workerTaskIndex
    byIdem sync.Map // 或 shard map
}

type taskShard struct {
    mu sync.RWMutex
    tasks map[string]*Task
}
```

对 worker running task 数、heartbeat 时间这类高频字段，可以单独放在 `workerRuntime`：

```go
type workerRuntime struct {
    runningTasks atomic.Uint32
    lastHeartbeat atomic.Int64
    cachedModels atomic.Value // immutable map snapshot
}
```

这样 `Heartbeat` 不需要阻塞 task map。

### clone 优化

原则：

1. 对外返回不可变 snapshot，内部使用 pointer。
2. 大的 `[]byte` 字段可以只在需要跨 goroutine 修改时复制。
3. 对 workflow state，step map 可以改为 slice + stepID index，减少 map clone。

workflow 当前使用 `map[string]StepState`。可以改成：

```go
type WorkflowState struct {
    StepOrder []string
    StepIndex map[string]int
    Steps []StepState
}
```

这样 schedule ready steps 时遍历 slice，cache locality 更好，序列化也更稳定。

### 执行步骤

1. 加 pprof：heap profile、alloc_space、mutex profile。
2. 为 `MemoryStore` 写并发 benchmark：GetTask、LeaseTask、CompleteTask、Heartbeat、ActiveWorkers、UpdateWorkflow。
3. 抽象 Store 接口不变，增加 `MemoryStoreV2` 实现。
4. 先拆 worker store，因为 heartbeat 是高频路径。
5. 再拆 task store，并增加 status index 和 running deadline index。
6. 最后优化 workflow/actor state 表示。
7. 用 race test 和现有 fault injection 验证。

### 验收指标

1. mutex profile 中 MemoryStore 的阻塞下降。
2. heap alloc/op 下降。
3. 高并发 worker heartbeat + task complete 下 p99 降低。
4. 语义测试不变。

---

## 9. P1 优化六：PostgreSQL materialized view 改成异步批量写

### 当前瓶颈

`PostgresStore` 当前先更新 memory，再同步执行数据库 upsert。这样每次 heartbeat、lease、complete 都可能触发一次 SQL。由于 shared log 已经是系统 source of truth，Postgres 更适合作为可重建的 materialized view，而不是所有状态变更的同步路径。

### 优化目标

把 PostgreSQL 写入从同步、逐条 upsert 改成异步 materializer：内存状态立即更新，变更事件进入 durable 或内存队列，后台批量 upsert。

### 设计方案

```go
type Materializer struct {
    queue chan metadataDelta
    batchMax int
    flushInterval time.Duration
    db *sql.DB
}

type metadataDelta struct {
    kind DeltaKind
    key string
    payload any
    version int64
}
```

1. 同一 key 的多次更新可以合并，只保留最后版本。
2. heartbeat 可以降频，比如每 1 秒批量 flush 一次，而不是每次 poll tick 写一次。
3. workflow/actor 的多个 step/command 更新可以在事务内批量提交。
4. `LastError` 仍保留，但不阻塞主路径。
5. control restart 时仍可从 shared log bootstrap，因此异步写丢失不会破坏 correctness。

### 执行步骤

1. 为 PostgresStore 增加 `mode=sync|async` 配置，默认先保留 sync。
2. 实现 Materializer goroutine。
3. 对 task/worker/model/workflow/actor persist 做批量版本。
4. heartbeat 降频合并。
5. 增加 dashboard 一致性检查：metadata view 与 replay snapshot 最终一致。
6. 增加 crash test：control 写 log 后 crash、Postgres 未 flush，重启后从 log rebuild。

### 验收指标

1. Compose 模式下 task throughput 和 control p99 改善。
2. PostgreSQL QPS 降低。
3. control crash 后 metadata 可恢复。
4. dashboard eventual consistency 延迟可观测。

---

## 10. P1 优化七：bootstrap/replay 增量 checkpoint

### 当前瓶颈

`BootstrapFromLog` 会 bootstrap models、workers、scheduler、backpressure、tasks、workflows、actors、LLM stats。任务和 workflow/actor 依赖 `ListStreams` + `ReadLog` + replay。日志规模增长后，重启时间会随历史日志线性增长。

### 优化目标

增加 metadata checkpoint，让 control 可以从 checkpoint + log tail 恢复，而不是每次从头 replay 所有 stream。

### 设计方案

新增系统 stream：

```text
system:checkpoints
```

checkpoint payload：

```json
{
  "checkpoint_id": "...",
  "created_at_ms": 123,
  "streams": {
    "wf:xxx": {"last_seq": 100, "state_ref": "local://..."},
    "actor:yyy": {"last_seq": 200, "state_ref": "local://..."},
    "task:zzz": {"last_seq": 3, "state_ref": "local://..."}
  }
}
```

或者拆成每类状态 checkpoint：

1. `system:task-checkpoints`
2. `system:workflow-checkpoints`
3. `system:actor-checkpoints`
4. `system:llm-stats-checkpoints`

### 执行步骤

1. 先为 LLM stats 实现 checkpoint，因为它现在会从 `llm:*` stream rebuild。
2. 再为 task specs 和 terminal task state 做 checkpoint。
3. workflow/actor 已有 replay reducer，适合直接保存 reducer state。
4. 在 bootstrap 中优先加载最新 checkpoint，再 read tail from lastSeq+1。
5. 加一致性校验：checkpoint replay state 与 full replay state 比较。
6. 引入 checkpoint retention，只保留最新 N 个。

### 验收指标

1. 重启时间不再和全量历史线性增长。
2. checkpoint 损坏时可回退 full replay。
3. state consistency 测试通过。

---

## 11. P1 优化八：physical compaction

### 当前瓶颈

actor snapshot 后有 logical trim，`ReadLog` 默认隐藏 trim point 前记录，但 segment 文件没有物理删除。长期运行后，磁盘空间不会实际释放。

### 优化目标

实现真正的 segment compaction：当 segment 中所有或大部分 records 都在 trim point 前，物理删除或重写 segment。

### 设计方案

分两阶段做：

第一阶段：segment-level delete。

1. 为每个 segment 维护 `liveBytes`、`totalBytes`、`min/max stream seq`。
2. 如果某 segment 所有 records 都小于对应 stream trimBefore，则可删除。
3. 删除前写 compaction manifest：

```json
{
  "compaction_id": "...",
  "deleted_segments": [1,2,3],
  "safe_before": {...}
}
```

第二阶段：copy compaction。

1. 对部分 live 的 segment，创建新 segment，只复制 live records。
2. 重建 index。
3. 原子切换 manifest。
4. 删除旧 segment。

### 执行步骤

1. 实现只读 compactability 统计，不删除文件。
2. 加入 segment-level delete，只处理完全可删除 segment。
3. 加 crash test：delete 前 crash、delete 后 manifest 未写、manifest 写后 crash。
4. 再做 copy compaction。
5. 加后台 compactor goroutine，限制 I/O 速率。

### 验收指标

1. actor snapshot 长跑后磁盘占用可下降。
2. compaction 不破坏 replay。
3. compaction 中断后可恢复。

---

## 12. P1 优化九：Python function/code registry

### 当前瓶颈

TaskSpec 携带 `function_source`。SDK 可能读取整个 module source；executor 每次 compile。对重复函数和 workflow 多 step，这会造成三类重复：gRPC payload、log payload、Python compile。

### 优化目标

把函数源码改成 content-addressable code blob：TaskSpec 只携带 `function_ref`，worker 通过本地 cache 或 object store 获取源码，并缓存 compile 结果。

### 数据结构建议

新增 proto 字段：

```proto
message TaskSpec {
  ...
  string function_ref = 24;
  string function_hash = 25;
}
```

新增 system stream：

```text
system:functions
```

函数注册事件：

```json
{
  "function_hash": "sha256:...",
  "source_ref": "local://functions/...",
  "language": "python",
  "entrypoint": "module:function"
}
```

worker cache：

```go
type FunctionCache struct {
    mu sync.RWMutex
    entries map[string]*CompiledFunction
}
```

Python executor 侧：

1. 用 hash 判断是否已 compile。
2. 复用 code object 或 module namespace。
3. module 级依赖尽量只 import 一次。

### 执行步骤

1. SDK 在 submit 时计算 source hash。
2. control 如果函数 hash 未注册，写 `FunctionRegistered` 并把源码写 object store。
3. TaskSpec 使用 function_ref + function_name。
4. worker 启动 FunctionCache。
5. Python executor 协议新增 `function_hash` 和 optional `source`。
6. benchmark：相同函数重复提交 1/100/1000 次，比较 payload size、compile time、latency。

### 验收指标

1. 重复 workflow submit 的 payload 和日志大小下降。
2. Python compile 占比下降。
3. 兼容旧 TaskSpec function_source。

---

## 13. P1 优化十：checkpoint cache per-model lock + O(1) LRU

### 当前瓶颈

worker 的 model cache 有全局 `checkpointMu`。多个模型并发冷启动会互相阻塞。LRU 如果通过 map 扫描找最旧 entry，模型数量上升后会退化。

### 优化目标

对每个 `(model, version)` 使用 singleflight/per-key lock，LRU 使用 list + map O(1) 维护。

### 数据结构建议

```go
type modelCache struct {
    mu sync.Mutex
    entries map[string]*list.Element
    lru *list.List
    inflight map[string]*loadCall
    usedBytes int64
    capacityBytes int64
}

type cacheEntry struct {
    key string
    path string
    size int64
    lastAccess int64
}

type loadCall struct {
    done chan struct{}
    result checkpointLoadResult
    err error
}
```

### 执行步骤

1. 把 global checkpointMu 替换成 per-key inflight。
2. LRU get 时 move-to-front。
3. put 后循环 evict back，直到容量满足。
4. manifest 更新用 temp file + rename。
5. 心跳上报 cache entries 时从 LRU snapshot 生成，不长时间持锁。

### 验收指标

1. 多模型 cold start 并发时互不阻塞。
2. 同一模型多个并发请求只触发一次 checkpoint fetch。
3. cache eviction 顺序可测试。

---

## 14. P1 优化十一：object store streaming 和原子本地写

### 当前瓶颈

`Store` 接口是 `Put(ctx, namespace, data []byte)` 和 `Get(ctx, ref) ([]byte, error)`，这会强制大结果和 snapshot 全量进内存。LocalStore 直接 WriteFile，缺少 temp rename 和 fsync；S3Store 整体 hash + 整体 request，缺少 multipart/streaming。

### 优化目标

把 object store 升级成 streaming 接口，同时增强 local durability。

### 新接口

```go
type Store interface {
    Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error)
    Get(ctx context.Context, ref string) (io.ReadCloser, ObjectInfo, error)
}
```

兼容层：

```go
func PutBytes(ctx context.Context, s Store, ns string, data []byte) (string, error)
func GetBytes(ctx context.Context, s Store, ref string, maxBytes int64) ([]byte, error)
```

### local atomic write

1. 写 `path.tmp.<pid>.<random>`。
2. 写完 `file.Sync()`。
3. `os.Rename(tmp, final)`。
4. 对 parent directory 执行 fsync。
5. 如果 final 已存在且 content hash 相同，可以跳过。

### S3 优化

1. ensureBucket 用 `sync.Once`，不要每次 Put 都发请求。
2. 大对象使用 multipart upload。
3. HTTP transport 设置连接池：`MaxIdleConnsPerHost`、`IdleConnTimeout`。
4. 支持 server-side checksum metadata。

### 验收指标

1. 大 result/snapshot 的峰值内存下降。
2. crash 后不会留下被引用但写坏的 local object。
3. S3 Put/Get p95 下降，bucket ensure 请求次数下降。

---

## 15. P2 优化十二：CRC32C、SIMD 与 checksum 策略

### 当前状况

log record 使用 CRC32 校验 payload body。CRC32 是正确的方向：它用于发现随机损坏，不用于安全认证。当前更重要的问题不是“有没有 CRC”，而是校验算法和编码路径是否与硬件和批处理匹配。

### 建议

1. 改用 CRC32C Castagnoli，多数 x86_64/ARM64 能走硬件加速路径。
2. 在 record header 中记录 checksum type：`IEEE`、`CRC32C`、`XXH3`、`None`。
3. 新老 record 兼容读取。
4. 对小 payload，函数调用和内存 copy 可能比 CRC 本身更显著，所以要和 buffer pool/zero-copy 一起 benchmark。
5. 对大 payload，可以考虑分块 checksum，支持 partial read 校验。

### 执行步骤

1. 新增 `ChecksumType` 枚举。
2. header 扩展 version 2。
3. 实现 `checksum(data []byte, typ ChecksumType)`。
4. 加 benchmark：payload 128B/1KB/64KB/1MB。
5. 加 corruption test，覆盖 header corruption、payload corruption、partial tail。

### 验收指标

1. CRC32C 在目标机器上对大 payload 更快或至少不慢。
2. 所有旧 segment 仍能读取。
3. corruption detection 不下降。

---

## 16. P2 优化十三：buffer pool、zero-copy-ish 和 JSON 开销下降

### 当前瓶颈

log record encode、task payload marshal、workflow args resolve、actor snapshot normalize、S3/local object store 都会制造大量 byte slice。并且 protobuf/gRPC 边界和 JSON payload 边界很难做到真正 zero-copy，但可以减少中间 copy。

### 建议

1. `sync.Pool` 管理 encode buffer，尤其是 log record header + body buffer。
2. 对 `ReadLog`，可以提供 `ReadLogRaw` 内部接口，replay reducer 直接消费 raw payload，减少 `Record` 对象构造。
3. workflow args hash 当前使用 JSON marshal 后 SHA256；可以缓存 step resolved args hash，避免 retry 时重复 marshal。
4. 对 TaskSpec、ActorEventPayload、WorkflowEventPayload 这类内部事件，考虑从 JSON 切换到 protobuf 或 msgpack。JSON 保留给 CLI/dashboard。
5. Python executor 协议可以从 JSON line 切换到 msgpack frame，降低序列化成本。

### 注意

不要一开始就全局替换 JSON。先 profile。如果 CPU 火焰图中 JSON marshal/unmarshal 占比明显，再逐步替换热点事件类型。

---

## 17. P2 优化十四：mmap read path 实验

### 判断

mmap 对 log read/replay 可能有收益，尤其是顺序 replay 和随机读取 record 时可以减少系统调用。但 mmap 写 WAL 的 crash consistency 更复杂，不建议作为第一阶段写路径。

### 建议范围

1. 只对 sealed segment 开启 mmap read。
2. active segment 继续用 `ReadAt`。
3. segment fd cache 升级成 segment mapping cache。
4. 读取 record 时在 mmap byte slice 上解析 header 和 body。
5. 需要注意 mmap 文件删除/compaction 的生命周期管理。

### 执行步骤

1. 实现 `SegmentReader` 接口：`ReadAt` 和 `MmapRead` 两种实现。
2. 增加 build tag 或 runtime config：`LOGSERVE_LOG_MMAP_READ=1`。
3. benchmark read/replay。
4. 做 Linux/macOS 兼容；Windows 先不启用。

### 验收指标

1. replay/read latency 有明确下降。
2. RSS、page fault、mmap count 可观测。
3. compaction 与 mmap 生命周期不冲突。

---

## 18. P2 优化十五：gRPC batch/streaming 和长轮询

### 当前瓶颈

worker 使用固定 poll interval。空闲时会空轮询；有任务时也可能等到下一个 tick。每次最多 poll 一个 task。

### 优化目标

减少 RPC 次数和调度延迟。

### 方案

1. `PollTask` 增加 `max_tasks`，支持批量返回多个 task。
2. 增加 long-poll：没有任务时 control 等待一小段时间或直到有 task 到达。
3. 增加 server-streaming：worker 建立 `TaskStream`，control 推送 task spec。
4. `CompleteTask` 增加 batch complete。
5. 心跳和任务拉取拆分，避免 poll tick 绑定 heartbeat。

### 执行步骤

1. 先做 `PollTask(max_tasks)`，复杂度最低。
2. worker 本地 pool 按空闲容量一次拉取 N 个。
3. control scheduler 支持一次 assign 多个。
4. 再做 long-poll，使用 condition variable 或 channel 通知新任务。
5. 最后评估 server-streaming。

### 当前落地状态

已实现 unary gRPC 路径上的 `PollTask(max_tasks, wait_timeout_ms)`、批量 `tasks` 响应、`CompleteTasks` 批量完成、worker 按本地空闲容量批量拉取、空闲 long-poll 等待任务通知，以及独立 heartbeat ticker。

server-streaming 暂不暴露为正式 `TaskStream` RPC。当前 batch + long-poll 已经去掉固定 tick 的主要空转和调度延迟；直接推 streaming 还需要额外定义流控、worker 断线重连、任务 lease 回收、per-worker backpressure 和观测指标。下一步只有在 unary batch 路径的 RPC/task 仍明显偏高，或需要 control 主动按 worker channel 推送时，再把 `TaskStream` 作为新的协议面加入。

### 验收指标

1. 低负载任务调度延迟下降。
2. 高负载 RPC count/task 下降。
3. worker capacity 利用率上升。

---

## 19. P2 优化十六：workflow step 表示从 map 改成 slice + index

### 当前瓶颈

workflow state 使用 `map[string]StepState`，调度 ready step 时遍历 definition steps，并查 map。小 DAG 无问题；复杂 DAG 或大量 workflow 并发时，map lookup、clone 和 JSON 序列化会变成开销。

### 方案

1. 保留外部 JSON 中的 step_id。
2. 内部 state 使用 `[]StepState`，按 topological order 存放。
3. `stepID -> index` 使用一次性 map。
4. 每个 step 存储 unresolved dependency count。
5. 当 step succeeded 时，只访问它的 outgoing edges，减少每次 scheduleReadySteps 全图扫描。

### 数据结构

```go
type RuntimeDAG struct {
    steps []StepState
    byID map[string]int
    outgoing [][]int
    remainingDeps []int
    ready deque[int]
}
```

### 执行步骤

1. 在 workflow.ParseDefinition 后构建 RuntimeDAG。
2. scheduleReadySteps 先使用 ready queue，而不是每次扫描所有 steps。
3. replay 时重建 DAG runtime index。
4. 对现有 map state 保持兼容，内部引入 runtime view。

### 验收指标

1. 大 DAG 调度 CPU 下降。
2. workflow retry/timeout/replay 语义不变。

---

## 20. P2 优化十七：actor mailbox 的数据结构优化

### 当前瓶颈

actor 有 per-actor lock，control 通过 `command_seq == command_count+1` 保证顺序。这个语义正确。优化空间在于 actor pending command 的索引和 actorLocks 的生命周期。

### 建议

1. actorLocks map 中的锁需要清理，避免 actor 数量长期增长后泄漏。
2. actor pending queue 由 scheduler 管理，不要和普通任务混在全局 queue。
3. actor state snapshot 可以增加 delta snapshot 或 command batch apply，减少每条 command 都携带完整 actor state 的成本。
4. 对高频 actor，可使用 actor-owner worker 本地 mailbox，control 只做 lease/fencing，减少控制面 round trip。这个方案复杂，但能体现 actor runtime 的深入优化能力。

### 执行步骤

1. 先在 scheduler v2 中引入 actorPending。
2. actorLocks 使用 refcount 或 TTL cleanup。
3. 对 actor command payload 增加 state size 统计。
4. 若 state 大且 command 高频，再设计 delta snapshot。

---

## 21. P2 优化十八：LLM scheduling 的 placement index 与预测模型

### 当前状况

locality-aware 已经能提升 cache hit。predicted-latency 用 materialized EWMA stats，不在热路径扫描 `llm:*` stream，这是正确方向。但每次调度仍然依赖 active worker scan。

### 优化目标

把 worker placement、capacity、model cache 变成增量维护的 scheduler state。

### 数据结构

```go
type WorkerRuntime struct {
    workerID string
    capacity uint32
    running atomic.Uint32
    cachedModels atomic.Value // immutable set
    lastHeartbeat atomic.Int64
}

type ModelPlacement struct {
    cached map[modelKey]workerHeap
    cold workerHeap
}
```

workerHeap 的 score 可包含：

1. available capacity。
2. cache hit。
3. EWMA latency。
4. recent queue wait。
5. eviction risk。

### 执行步骤

1. heartbeat 更新 placement index。
2. LLMCompleted 更新 EWMA 后调整 heap score。
3. PollTask 不再每次扫描全部 active workers。
4. benchmark workers = 10/100/1000。

### 验收指标

1. worker 数扩大时 scheduling CPU 更稳定。
2. cache hit rate 不下降。
3. predicted-latency 选择稳定，不出现频繁抖动。

---

## 22. P2 优化十九：benchmark、profile 和回归门禁

### 必须先补的基准

否则优化无法判断真实收益。

1. logstore append microbenchmark：payload size、stream count、fsync policy、batch size。
2. logstore read microbenchmark：fromSeq 位置、limit、segment count、mmap/fd cache。
3. control scheduler benchmark：queue depth、worker count、actor/LLM/general 混合比例。
4. metadata benchmark：Get/Lease/Complete/Heartbeat/WorkflowUpdate 并发。
5. bootstrap benchmark：stream count、record count、checkpoint on/off。
6. checkpoint cache benchmark：model count、checkpoint size、cold/warm/mixed。
7. Python executor benchmark：same function repeated、module source size、compile cache on/off。

### profile 建议

在实验脚本中自动收集：

```bash
curl http://127.0.0.1:<debug-port>/debug/pprof/profile?seconds=30 > cpu.pprof
curl http://127.0.0.1:<debug-port>/debug/pprof/heap > heap.pprof
curl http://127.0.0.1:<debug-port>/debug/pprof/mutex > mutex.pprof
curl http://127.0.0.1:<debug-port>/debug/pprof/block > block.pprof
```

Go 程序启动时打开：

```go
runtime.SetMutexProfileFraction(10)
runtime.SetBlockProfileRate(10000)
```

### 回归门禁

每个优化 PR 必须给出：

1. correctness test。
2. microbenchmark 前后对比。
3. macro benchmark 前后对比。
4. pprof 证据。
5. 失败回滚开关。

---

## 23. 不建议优先做的优化

### 23.1 DPDK / kernel bypass

当前系统主要瓶颈更可能在本地 log I/O、control queue、metadata lock、Python executor 和 object store，而不是 kernel network stack。gRPC 请求粒度也不是高频小包 L2/L3 转发场景。DPDK 会极大增加复杂度，没有合理收益。

### 23.2 CPU affinity

除非 benchmark 显示 goroutine migration 或 cache miss 是瓶颈，否则不值得先做。Go runtime 调度和 gRPC/文件 I/O 结合时，强绑核可能反而降低调度弹性。

### 23.3 NUMA

只有多 socket、大内存、大模型 cache 或真实 vLLM/GPU 负载下才值得考虑。当前单机 mock LLM 和小 checkpoint 不适合优先投入。

### 23.4 huge page

对 Go control/logstore 的小对象和文件 I/O 没有明显第一性收益。LLM KV cache 或大模型权重映射可能受益，但这属于 vLLM/GPU serving 层，不是当前 control/logstore 的首要优化。

### 23.5 Direct I/O

Direct I/O 绕过 page cache，会使编码、对齐、buffer 管理和小写入复杂化。当前 log read、recovery 和 checkpoint cache 很可能受益于 page cache。除非后续证明 page cache 污染严重，否则不建议主路径使用。

### 23.6 io_uring

io_uring 可能改善 Linux 上的异步文件 I/O，但 Go runtime、gRPC 和跨平台兼容会增加复杂度。建议先把 batch write、group commit、fd cache 做完，再作为 Linux-only 实验。

### 23.7 lock-free queue

control 的问题不是单纯队列 push/pop 成本，而是队列模型不对：全局 queue + 线性扫描。先改 indexed scheduler。等数据结构正确后，如果锁 profile 仍显示 scheduler lock 是瓶颈，再考虑 lock-free 或多分片队列。

---

## 24. 推荐路线图

### Phase 0：建立可量化基线

周期：1-2 天。  
目标：让所有后续优化有证据。

任务：

1. 增加 `benchmarks/` 下的 Go microbench：logstore append/read/recovery、metadata、scheduler。
2. 给 logd/control/worker 增加可选 pprof endpoint。
3. 实验脚本自动输出 CPU/heap/mutex/block profile。
4. 报告中增加每项优化前后的对比表。

交付物：

1. `benchmarks/baseline-<timestamp>.json`
2. `reports/profile-<timestamp>/`
3. 文档化基线。

### Phase 1：logstore 写读路径

周期：3-6 天。  
目标：解决最底层 I/O 热点。

任务：

1. ReadLog 二分查找。
2. segment fd cache。
3. Append writer loop。
4. AppendBatch proto。
5. group commit。
6. CRC32C 和 checksum type。
7. crash/recovery 测试。

交付物：

1. logstore benchmark 提升报告。
2. batch crash test。
3. 兼容旧 segment 的读取测试。

### Phase 2：control scheduler v2

周期：4-8 天。  
目标：消除队列线性扫描和 redelivery 全表扫描。

任务：

1. Scheduler 接口抽象。
2. typed queues。
3. LLM placement index。
4. actor pending queue。
5. running deadline heap。
6. PollTask batch。
7. fallback 开关。

交付物：

1. queue depth 1k/10k/100k benchmark。
2. actor/LLM/general 混合 workload benchmark。
3. fault injection 全通过。

### Phase 3：metadata store v2 和 Postgres async

周期：4-7 天。  
目标：降低 metadata 锁竞争和 SQL 写放大。

任务：

1. worker store 拆分。
2. task store sharding。
3. status/running indexes。
4. workflow state slice/index runtime view。
5. async materializer。
6. dashboard eventual consistency 指标。

交付物：

1. metadata mutex profile 对比。
2. Postgres QPS 对比。
3. crash + rebuild 测试。

### Phase 4：recovery checkpoint 和 compaction

周期：6-12 天。  
目标：解决长期运行后的启动时间和磁盘空间。

任务：

1. LLM stats checkpoint。
2. task/workflow/actor checkpoint。
3. checkpoint + tail replay。
4. segment-level physical delete。
5. copy compaction 实验。
6. compaction crash safety。

交付物：

1. bootstrap time vs log size 曲线。
2. compactable bytes vs physical reclaimed bytes 曲线。
3. compaction recovery 测试。

### Phase 5：worker/cache/object store 优化

周期：4-8 天。  
目标：降低 Python 重复 compile、checkpoint cold start 阻塞和大对象内存峰值。

任务：

1. function/code registry。
2. Python executor compile cache。
3. checkpoint per-key singleflight。
4. O(1) LRU。
5. object store streaming。
6. S3 ensureBucket once 和 HTTP transport tuning。

交付物：

1. repeated function workflow benchmark。
2. multi-model cold start benchmark。
3. large snapshot/result memory profile。

### Phase 6：高级 I/O 实验

周期：可选。  
目标：只在 P0/P1 完成后验证更底层技术。

候选：

1. mmap read path。
2. io_uring Linux-only log reader/writer 实验。
3. Direct I/O 对比。
4. CPU affinity/NUMA on multi-socket。

准入条件：

1. 已有 profile 证明 syscall/page cache/NUMA 是瓶颈。
2. 有 feature flag。
3. 有回退路径。

---

## 25. 面试展示建议

如果你把这些优化做一部分，会非常适合面试中展示“从能跑到能扩展”的能力。建议优先选择下面三个组合：

### 组合 A：logstore 工程深度

内容：AppendBatch + group commit + binary index + CRC32C + crash recovery。  
面试价值：WAL、fsync、page cache、checksum、恢复、批量提交、二进制格式、性能测试。

### 组合 B：scheduler 系统设计深度

内容：从 `[]string` 线性队列升级到 typed/indexed scheduler + deadline heap + LLM placement index。  
面试价值：数据结构选型、调度复杂度、并发控制、backpressure、actor mailbox、LLM locality。

### 组合 C：replay/compaction 长期运行能力

内容：checkpoint + tail replay + physical compaction。  
面试价值：event sourcing、日志压缩、snapshot、恢复一致性、crash safety。

最推荐先做组合 A 和 B。它们最贴近基础架构，并且收益清晰，能直接回应“你如何优化系统瓶颈”的面试追问。

---

## 26. 最小可执行任务清单

下面是可以直接拆 issue/PR 的任务列表。

### logstore

1. `BenchmarkStoreAppendSingleStream`。
2. `BenchmarkStoreAppendMultiStream`。
3. `BenchmarkStoreReadFromTail`。
4. `BenchmarkStoreRecoverLargeSegments`。
5. `Read` 改为 `sort.Search`。
6. 增加 segment fd cache。
7. 新增 binary index v2。
8. 新增 checksum type。
9. CRC32C Castagnoli。
10. writer goroutine。
11. AppendBatch proto。
12. group commit。
13. batch crash tests。
14. physical segment delete compaction。

### control scheduler

1. 封装 Scheduler 接口。
2. typed queues。
3. target worker queue。
4. actor pending queue。
5. LLM model placement index。
6. running deadline heap。
7. PollTask(max_tasks)。
8. Long-poll optional。
9. scheduler v2 feature flag。
10. queue-depth benchmark。

### metadata

1. MemoryStore benchmark。
2. worker store 拆分。
3. task shard map。
4. task status index。
5. running deadline index。
6. clone 减少。
7. workflow runtime DAG。
8. actor lock cleanup。
9. Postgres async materializer。
10. batch upsert。

### worker/cache

1. PollTask batch client。
2. executor queue metrics。
3. Python function hash。
4. function registry。
5. Python compile cache。
6. checkpoint per-model lock。
7. O(1) LRU。
8. manifest atomic write。
9. S3 ensureBucket once。
10. object store streaming。

### recovery/compaction

1. LLM stats checkpoint。
2. task checkpoint。
3. workflow checkpoint。
4. actor checkpoint。
5. checkpoint + tail replay。
6. checkpoint corruption fallback。
7. segment live-bytes accounting。
8. segment-level delete。
9. copy compaction。
10. compaction crash safety。

---

## 27. 结论

LogServe 当前最有价值的优化不是直接堆硬件技巧，而是先修基础数据结构和 I/O 模型：

1. logstore：batch、group commit、二分 index、fd cache、binary index、CRC32C。
2. control：indexed scheduler、typed queues、deadline heap、batch poll。
3. metadata：sharding、状态索引、减少 clone、异步 PostgreSQL materialization。
4. recovery：checkpoint + tail replay。
5. retention：physical compaction。
6. worker：function registry、compile cache、checkpoint cache LRU。

等这些完成后，再用 profile 判断是否需要 mmap、io_uring、Direct I/O、CPU affinity、NUMA、huge page 等更底层优化。
