# LogServe 技术架构

LogServe 是一个 shared-log-based AI runtime。它把系统中的关键状态变化都写成事件，放进 append-only shared log，然后由控制面把这些事件 materialize 成当前视图。这样做的好处是：状态变化有来源，服务重启后可以 replay，workflow、actor 和 LLM 调度都能用同一套恢复模型解释。

## 总体结构

```text
Python SDK
  | submit task / workflow / actor / llm request
  v
control plane
  | append events first
  v
logd shared log  <---- replay / bootstrap
  |
  v
metadata view / dashboard
  |
  v
workers -> Python executor / mock LLM / vLLM adapter / checkpoint cache
```

项目里有四类核心进程：

| 进程 | 作用 |
|---|---|
| `logd` | 提供分段 append-only log。支持 stream 读写、幂等 append、恢复、checksum、trim 和 compaction。 |
| `control` | 接收 SDK 请求，写日志，调度任务，维护 workflow、actor、LLM 和 worker 的当前视图。 |
| `worker` | 心跳、拉取任务、本地排队、执行 Python 函数、调用 mock/vLLM、管理模型 checkpoint cache。 |
| `logservectl` / Python SDK | 给 Python 用户暴露 `@task`、`@workflow`、`@actor` 和 `llm_generate()`。 |

## shared log 是状态源

LogServe 的关键设计是 log-first。控制面处理请求时，不是先改内存再补日志，而是先写事件：

```text
TaskSubmitted -> TaskStarted -> TaskCompleted / TaskFailed
WorkflowStarted -> StepScheduled -> StepSucceeded / StepFailed
ActorCreated -> ActorOwnershipGranted -> ActorCommandSubmitted -> ActorCommandApplied
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

metadata view 只是当前状态的投影。它可以存在内存里，也可以落到 PostgreSQL。只要 shared log 还在，控制面重启后就可以重新 replay，恢复 task、workflow、actor、worker、model 和 LLM stats。

shared log 的实现重点：

- 每个 stream 独立递增 seq。
- append 支持 idempotency key，重复请求不会写出第二条相同事件。
- segment 文件按大小滚动。
- 新记录默认使用 CRC32C checksum，旧 segment 仍可读取。
- `ReadRawEach` 和 `ReadLogStream` 支持边读边 replay，避免把大量日志一次性装进内存。
- actor snapshot 后可以 logical trim，`ReadLog` 默认跳过 trim point 之前的记录。
- physical compaction 可以删除完全 trim 的 segment，也可以 copy live records 重写部分 segment。

## control plane

control plane 是项目的调度和状态中心。它主要做六件事：

1. 接收 SDK 或 CLI 请求。
2. 生成 task、workflow、actor、LLM 事件并写入 shared log。
3. 更新 metadata view。
4. 根据 worker 心跳和容量分配任务。
5. 在 worker 失败后 redeliver。
6. 在重启时从 log 或 metadata checkpoint 恢复状态。

控制面当前支持两类 metadata view：

| 模式 | 用法 |
|---|---|
| in-memory | 本地开发和测试默认路径，启动快，便于单元测试。 |
| PostgreSQL | Docker Compose 环境使用。PostgreSQL 不是状态真相，而是 dashboard 和查询使用的 materialized view。 |

PostgreSQL 写入可以走同步模式，也可以走 async materializer。async 模式先更新内存 view，再把 delta 放入后台队列批量 flush 到数据库。这样主路径不被每一次 SQL upsert 卡住。验收结果显示 async 模式明显降低了数据库 transaction/write rate，并且 task throughput 和 p99 保持在非退化阈值内。

## scheduler v2

旧调度模型把所有 task 放在一个队列里，worker poll 时从头扫描。任务类型多、backlog 深、worker 数多时，这个模型会浪费很多判断成本。

scheduler v2 把任务按调度语义分开：

| 队列或索引 | 处理什么 |
|---|---|
| general queue | 普通 task。 |
| target-worker queue | 必须投递给指定 worker 的任务。 |
| `actorPending[actor_id]` | actor command。按 actor 分队列，避免和普通任务互相阻塞。 |
| LLM placement index | LLM 请求。按模型缓存、worker 容量和历史延迟选择 worker。 |
| deadline / lease tracking | 处理 redelivery，减少重复全表扫描。 |

worker 侧也做了配套优化：

- `PollTask(max_tasks, wait_timeout_ms)` 支持按本地空闲容量批量拉取。
- 空闲时使用 long-poll，不再靠固定 tick 反复空轮询。
- `CompleteTasks` 把一批完成结果合成一个 RPC 返回。
- heartbeat ticker 和任务 poll 拆开。

server-streaming `TaskStream` 暂时没有作为正式协议暴露。当前 unary batch + long-poll 已经解决了主要空转和调度等待问题；如果后续 RPC/task 仍偏高，再引入 streaming 会更稳。

## workflow runtime

Python SDK 的 `@workflow` 会把函数调用轨迹转成 DAG。控制面只调度依赖已经满足的 step：

```text
SCHEDULED -> STARTED -> SUCCEEDED
                     -> FAILED -> retry / terminal failure
```

workflow 的 source of truth 是 `wf:<workflow_id>` stream。metadata 中的状态只是当前视图。系统支持：

- step 依赖解析和 ready queue。
- retry 和 timeout。
- 大结果写入 result store，日志里只放 `result_ref`。
- worker 失败后 redelivery。
- replay 校验 metadata view。
- `workflow_id + step_id + input_hash` 级别的结果去重。

这里的语义是 exactly-once-ish。worker 可能至少执行一次，但控制面避免同一个 step/input 的最终结果被重复应用。项目不宣称严格 distributed exactly-once。

workflow 内部已经从 map 扫描改成 `RuntimeDAG`：

```text
step_id -> index
steps in topological order
remainingDeps
outgoing edges
ready queue
```

这样 step 成功后只需要更新它的 downstream，不用每次扫描整个 DAG。

## actor runtime

actor 的状态流是 `actor:<actor_id>`。控制面会写：

```text
ActorCreated
ActorOwnershipGranted
ActorCommandSubmitted
ActorCommandApplied
ActorSnapshotCreated
```

核心语义有三个：

1. mailbox 串行化：同一 actor 的 command 按 `command_seq` 应用。
2. ownership：每个 actor 有当前 owner worker。
3. epoch fencing：owner 变化时 epoch 增加，旧 worker 的完成会被拒绝。

actor snapshot 写入 result store，日志只保留 `snapshot_ref`。replay 时先加载最近 snapshot，再回放 snapshot 之后的 command。snapshot 创建后，logstore 会记录 trim point；默认读取 actor stream 时跳过旧记录。physical compaction 再根据 trim 信息删除或重写 segment。

## LLM serving

LLM 模块不是直接替代 vLLM，而是把 LLM 请求纳入同一套调度和恢复框架。

当前能力：

- model registry：记录模型名、版本、大小、路径和 adapter。
- mock LLM adapter：没有 GPU 时模拟 model load 和 first-token latency。
- vLLM adapter：调用 OpenAI-compatible `/v1/chat/completions`。
- worker model cache：worker 注册和心跳时上报本地模型缓存。
- file-backed checkpoint cache：冷请求从 source dir 拷贝 checkpoint 到 worker-local cache，热请求命中本地缓存。
- LLM event stream：`ModelLoadStarted -> ModelLoaded -> LLMCompleted`。
- `ReplayLLM`：重建 cache hit、fetch time、model load、first-token 和 total latency。

调度策略有三种：

| 策略 | 行为 |
|---|---|
| `RESOURCE_ONLY` | 只看 worker 是否有空闲容量。 |
| `LOCALITY_AWARE` | 优先把请求发给已经缓存该模型的 worker。 |
| `PREDICTED_LATENCY` | 使用 `LLMCompleted` 维护的 EWMA stats，估算总延迟后选择 worker。 |

预测延迟不是在线扫描所有 `llm:*` 日志，而是使用 materialized stats：

```text
predicted_latency =
  ewma_total_latency_ms
  + queue_penalty
  + cold_start_penalty
  + eviction_penalty
```

## Python SDK 和 executor

SDK 对用户暴露的是 Python 装饰器：

```python
@task
def embed(query: str):
    ...

@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    return generate_mock(query, docs)
```

SDK 默认使用 native gRPC；如果缺少 `grpcio` 和 `protobuf`，可以 fallback 到 `logservectl`。提交默认不是幂等的。如果调用者显式传 `idempotency_key`，控制面会检查 payload 是否一致；同一个 key 搭配不同 payload 会返回冲突。

worker 侧通过 Python executor 执行函数。executor IPC 默认使用 length-prefixed msgpack frame，必要时可以用 `LOGSERVE_EXECUTOR_PROTOCOL=json` 回退到 JSON lines。

## result store 和 object store

workflow 大结果、actor snapshot、模型 checkpoint 这类大对象不直接塞进日志。日志只保存引用，真实对象写到 result/object store。

当前有两类适配：

- local store：本地文件系统，适合开发和测试。
- S3-compatible store：可接 MinIO，Compose 环境会使用 MinIO。

本地 store 和 S3 store 都用于说明一个边界：shared log 存状态事件和引用，大对象走对象存储路径。

## dashboard 和观测

dashboard 读取 materialized view，展示 task、workflow、actor、worker、model cache、compactable log records/bytes 等状态。它不是状态源，但可以用来检查当前 view 是否和 replay 结果一致。

实验脚本会输出：

- command status
- benchmark JSON
- dashboard snapshot
- dashboard replay consistency
- fault injection 结果
- logstore benchmark
- metadata/scheduler benchmark
- pprof 文件

## 边界

当前实现验证的是单机多进程机制，不是生产级分布式平台。正确的表述是：

- 可以说明 shared log、replay、scheduler、actor fencing、metadata checkpoint、physical compaction 等机制已经跑通。
- 可以说明单机 Ubuntu 验收通过，主要回归门禁通过。
- 不能说明多机生产性能。
- 不能用 mock LLM 结果代表真实 GPU/vLLM 性能。
- 小 checkpoint cache 探针只能证明路径正确，不能推断大模型冷启动曲线。
