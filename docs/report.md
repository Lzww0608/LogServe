# LogServe Report

## 摘要

LogServe 是一个基于 shared log 的 AI runtime。它包含任务执行、workflow DAG、actor 状态恢复、LLM serving、locality-aware scheduling、故障恢复和 benchmark 分析。系统采用 log-first 路径：控制面先写事件日志，再更新 materialized metadata view；workflow、actor、LLM 状态都可以从日志 replay 重建。

代码现在包括 Python SDK、Go 控制面、worker 和 logd。Python SDK 支持 `@task`、`@workflow`、`@actor`；控制面负责调度、幂等、重试、actor mailbox 和模型调度；worker 负责本地 executor pool、Python 函数执行、mock/vLLM LLM 调用和 checkpoint cache；logd 提供可恢复的 append-only shared log。

## 系统实现

### 组件

- `logd`：分段 append-only shared log，支持 stream 读写、idempotent append、启动恢复和不同 fsync 策略。
- `control`：任务、workflow、actor、model、backpressure 的控制面；维护 materialized metadata view。
- `worker`：注册、心跳、poll，将 task/actor/LLM 分发到本地 executor pool，并写回完成事件。
- Python SDK：提供 `@task`、`@workflow`、`@actor`、LLM API；优先使用 gRPC，缺少依赖时 fallback 到 CLI。
- Result store：大结果和 actor snapshot 通过本地/S3-compatible MinIO 边界保存，日志只保留引用。
- Dashboard API：导出 queue、task、workflow、actor、worker、model cache 的当前视图。

### Workflow Runtime

Workflow 由 Python `@workflow` DSL 转成 DAG step model。控制面根据依赖关系调度 ready steps，step 状态为：

```text
SCHEDULED -> STARTED -> SUCCEEDED | FAILED
```

系统支持 retry、timeout、result ref、replay 校验和 step 级幂等。幂等键使用：

```text
workflow_id + step_id + input_hash
```

语义是 exactly-once-ish：worker 可能至少执行一次，但控制面避免重复提交同一 step 的最终结果。系统不声明严格 distributed exactly-once。

### Actor Runtime

Actor 以 `actor:<actor_id>` stream 为状态真相。控制面写入：

```text
ActorCreated -> ActorOwnershipGranted -> ActorCommandSubmitted -> ActorCommandApplied -> ActorSnapshotCreated
```

每个 actor command 分配单调递增 `command_seq`。同一 actor 的请求通过 mailbox 串行化，只有 `command_seq == actor.command_count + 1` 的命令可以被应用。Actor ownership 使用 `owner_worker_id + epoch` 做 fencing，旧 worker 或旧 epoch 的完成会被拒绝。

Snapshot 定期写入 result store；replay 优先从 snapshot 恢复，再回放 snapshot 之后的 command。Actor snapshot 创建后，控制面会调用 logstore 的 logical trim，将 actor stream 中 snapshot 之前的记录标记为 compactable。默认 `ReadLog` 从 trim point 之后读取，因此 actor replay 从 `ActorSnapshotCreated`、snapshot object 和 tail log 开始。目前没有物理删除 segment；系统会报告 compactable records/bytes，作为 physical compaction 的输入。

### LLM Serving

LLM 模块实现：

- Model Registry：记录模型名、版本、大小、路径和 adapter。
- Mock LLM：无 GPU 环境下模拟 model load 和 first token latency。
- vLLM Adapter：可调用 OpenAI-compatible `/v1/chat/completions`。
- Model Cache Manager：worker 注册/心跳上报本地模型缓存。
- Checkpoint Cache：冷启动从 `--model-source-dir` 拷贝 checkpoint 到 worker-local cache，热启动命中本地缓存。
- Scheduler：实现 `RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 三种策略。`PREDICTED_LATENCY` 使用由 `LLMCompleted` 事件增量维护的 materialized stats，按 `(model_name, model_version, worker_id)` 记录 request count、cache-hit count、EWMA total/model-load/checkpoint-fetch latency 和 last update time，调度时只做 `O(number_of_workers)` 查询，不在热路径扫描 `llm:*` streams。控制面现在还支持 metadata checkpoint：后台写入 `system:checkpoints` 后，重启优先从 checkpoint 恢复 LLM stats、task specs/terminal state、workflow state 和 actor state，再从各 stream 的 `last_seq+1` 读取 tail；没有可用 checkpoint 时回退 full replay。

LLM 请求写入 `llm:<task_id>` stream：

```text
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

`ReplayLLM` 可重建模型版本、worker、cache hit、checkpoint fetch、cache bytes、model load、first-token 和 total latency。

Predicted-latency 的估算公式为：

```text
predicted_latency =
  ewma_total_latency_ms
  + queue_penalty
  + cold_start_penalty
  + eviction_penalty
```

## 实验环境

实验在用户单机 Ubuntu 服务器上完成，使用 Docker Compose 在一台机器上模拟多 worker 部署：

```text
Project path: /home/lab2439/Work/lzww/LogServe
Run directory: reports/experiment-exp1782144454
Package: reports/experiment-exp1782144454/experiment-package.tar.gz
Mode: compose
Runtime: PostgreSQL, MinIO, logd, control, 3 workers
LLM: mock LLM, worker-local file-backed checkpoint cache
Scheduler: LOGSERVE_SCHEDULER_V2=1
```

这组实验用于验证单机多进程环境中的机制正确性和可复现性，覆盖 log-first、replay、redelivery、actor recovery、snapshot、typed/indexed scheduler、LLM locality、checkpoint cache 和 dashboard materialization。它不用于声明多机生产性能，也不等价于真实 GPU/vLLM 压测。

## 验证结果

### 基础验证

最终实验判定为 `PASS`。自动汇总脚本给出的验收项如下：

| 验收项 | 结果 |
|---|---:|
| all recorded commands pass | PASS |
| logstore relaxed fsync faster than always | PASS |
| locality cache hit not worse than resource-only | PASS |
| checkpoint warm cache hit | PASS |
| actor snapshot replay less than full replay | PASS |
| dashboard has three workers | PASS |

主要命令和探针均通过：

| 验证项 | 结果 | 用时 |
|---|---:|---:|
| Python venv create | PASS | 2 s |
| Python dependency install | PASS | 3 s |
| `go test -count=1 ./...` | PASS | 27 s |
| `go vet ./...` | PASS | 1 s |
| `go test -race -count=1 ./internal/control ./internal/metadata ./internal/worker` | PASS | 7 s |
| Python unittest | PASS | 0 s |
| Python compileall | PASS | 0 s |
| gRPC dependency check | PASS | 0 s |
| `logservectl` build | PASS | 1 s |
| scheduler benchmark | PASS | 7 s |
| metadata benchmark | PASS | 9 s |
| logstore benchmark | PASS | 12 s |
| fault injection tests | PASS | 6 s |
| compose build | PASS | 2 s |
| runtime compose start | PASS | 1 s |
| runtime logd/control/workers ready | PASS | 0 s |
| runtime benchmark | PASS | 45 s |
| checkpoint cache probe | PASS | 1 s |
| checkpoint artifact check | PASS | 0 s |
| dashboard snapshot | PASS | 0 s |
| summary and package generation | PASS | 0 s |

### PostgreSQL Async Materializer Acceptance

PostgreSQL metadata view 的 async materializer 在单机 Ubuntu Docker Compose 环境下完成了 sync/async 对比验收。对比结果来自：

```text
Run directory: reports/ubuntu-postgres-async-20260623T121546Z/postgres_async_compare
Summary: reports/ubuntu-postgres-async-20260623T121546Z/postgres_async_compare/summary.md
Mode: sync vs async PostgreSQL metadata materialization
Acceptance: PASS
Thresholds: task throughput ratio >= 0.99, task p99 ratio <= 1.0
```

核心指标如下：

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput | 5.08 tasks/s | 5.03 tasks/s | 0.9902 |
| Task submit p99 | 209 ms | 209 ms | 1.0000 |
| PostgreSQL tx/s | 72.629 | 1.329 | 0.0183 |
| PostgreSQL row writes/s | 101.423 | 17.083 | 0.1684 |

验收项全部通过：

| Check | Result |
|---|---:|
| task throughput within tolerance | PASS |
| task submit p99 within tolerance | PASS |
| PostgreSQL transactions per second reduced | PASS |
| PostgreSQL row writes per second reduced | PASS |
| async materializer mode observed | PASS |
| async materializer flush errors zero | PASS |

Dashboard replay consistency 也通过：sync 和 async 两组均检查了 5 个 workflow 与 2 个 actor，共 7 个对象，`failures=[]`。async dashboard snapshot 中 materializer 状态为 `mode=async`、`pending_deltas=6`、`flush_errors=0`、`eventual_lag_estimate_ms=840`。这说明 PostgreSQL view 在 async 模式下保持 eventual consistency，且未出现后台 flush error。

结论是：async materializer 明显降低 PostgreSQL 同步写压力，事务速率降到 sync 的 1.83%，行写入速率降到 16.84%；task throughput 和 p99 在默认非退化容差内保持稳定。本轮没有观察到 task throughput 或 p99 的严格改善，因此应表述为“数据库写入压力显著降低，主路径性能未明显退化”，而不是“任务吞吐显著提升”。

### Metadata Checkpoint Bootstrap Acceptance

metadata checkpoint 的单机 Ubuntu 验收已经通过。该测试用 in-memory shared-log harness 生成 task、workflow、actor 和 LLM metadata history，写入 `system:checkpoints`，再追加 tail 事件，对比 full replay 与 checkpoint-plus-tail replay。

```text
Run directory: reports/ubuntu-checkpoint-20260623T154803Z/checkpoint_acceptance
Summary: reports/ubuntu-checkpoint-20260623T154803Z/checkpoint_acceptance/summary.md
Acceptance: PASS
Workload: 120 tasks, 12 workflows, 12 actors, 40 llm streams, 68 tail events
```

核心结果如下：

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration | 3.759 ms | 2.327 ms | 0.6190 |

checkpoint payload 覆盖 196 个 stream、132 个 task、12 个 workflow、12 个 actor 和 2 条 LLM stats entry。一致性检查结果为 `consistent=true`，共检查 156 个对象。`checkpoint_created`、`checkpoint_read_records_reduced`、`checkpoint_replay_consistent`、`checkpoint_tail_only_reads`、`corrupt_checkpoint_fallback` 和 `checkpoint_retention` 全部通过。

结论是：metadata checkpoint 可以把 control restart 从全量历史扫描改为 checkpoint + log tail replay，并保持 metadata view 与 replay snapshot 一致。这次 workload 中读取记录数从 614 降到 71，说明历史记录扫描量明显下降。`ReadLog` 调用数只从 224 降到 201，因为重启路径仍要检查各 stream tail；因此这里的收益应表述为“减少历史 records 读取”，而不是“消除 stream 访问”。duration 从 3.759 ms 降到 2.327 ms，可作为本轮单机结果，但不应外推为多机生产恢复延迟。

### Runtime Benchmark

本次 compose 端到端 benchmark 结果如下：

| 指标 | 结果 |
|---|---:|
| Workflow p95 latency | 1244 ms |
| Workflow p99 latency | 1244 ms |
| Task throughput | 4.870 tasks/s |
| Task p99 latency | 521 ms |
| Actor full replay commands | 21 |
| Actor no-snapshot replay commands | 21 |
| Actor snapshot replay commands | 1 |
| Actor trimmed replay commands | 1 |
| Actor compactable log records | 45 |
| Actor compactable log bytes | 18,283 |
| LLM cold total latency | 98 ms |
| LLM warm total latency | 18 ms |
| LLM warm cache hit | true |
| LLM cold checkpoint fetch | 1 ms |
| LLM warm checkpoint fetch | 0 ms |

Actor snapshot ablation 显示 snapshot 和 logical trim 生效：同样 20 次 command 后，snapshot replay 和 trimmed replay 只需回放 1 条 command，而无 snapshot 需要回放 21 条。Dashboard 和 benchmark 同时报告 compactable records/bytes，用于衡量 snapshot-aware retention 可以释放的日志空间。

### Locality Scheduling Ablation

| 策略 | Cache hit rate | p95 latency |
|---|---:|---:|
| Resource-only | 1.000 | 209 ms |
| Locality-aware | 1.000 | 209 ms |
| Predicted-latency | 1.000 | 209 ms |

这次 workload 中三种策略都命中缓存，因此 locality-aware 和 predicted-latency 没有表现出额外 latency 差距。验收意义是：在 typed scheduler 和模型缓存索引开启后，locality-aware 的 cache hit 不低于 resource-only，且不会破坏 LLM 任务调度。若要证明 locality 策略在冷/热缓存不均衡场景下的收益，需要增加更强的冷启动扰动、模型分布差异或更大的 checkpoint。

### Checkpoint Cache Probe

checkpoint cache 探针结果如下：

| 指标 | Cold | Warm |
|---|---:|---:|
| cache hit | false | true |
| checkpoint fetch | 1 ms | 0 ms |
| worker | worker-a | worker-a |
| cache used | 2,097,152 bytes | 2,097,152 bytes |
| cache capacity | 16,777,216 bytes | 16,777,216 bytes |
| validation errors | none | none |

Artifact check 也通过，说明 file-backed checkpoint cache 写到了 worker-local cache，而不是只创建 source checkpoint。由于测试 checkpoint 较小，fetch/load 延迟主要用于确认功能路径，不用于推断大模型真实冷启动时间。

### Logstore Benchmark

20,000 records、16 streams、256-byte payload 的单机结果：

| fsync policy | Append records/s | Read records/s | Recover ms | Segments |
|---|---:|---:|---:|---:|
| always | 1,734.660 | 448,881.025 | 42 | 7 |
| batch | 285,940.289 | 656,219.478 | 44 | 7 |
| interval | 334,596.961 | 700,969.374 | 41 | 7 |

结果和写入策略一致：`always` 强同步写入最慢；`batch` 和 `interval` 的 append throughput 明显更高。恢复时间保持在几十毫秒量级。

### Scheduler And Metadata Microbenchmarks

Scheduler mixed backlog microbenchmark 覆盖 `queue_depth = 1k/10k/100k` 和 `worker = 1/10/100`：

| queue depth | workers | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|---:|
| 1,000 | 1 | 537.1 | 105 | 2 |
| 1,000 | 10 | 2,305 | 367 | 4 |
| 1,000 | 100 | 16,946 | 2,480 | 6 |
| 10,000 | 1 | 534.2 | 128 | 2 |
| 10,000 | 10 | 1,759 | 344 | 3 |
| 10,000 | 100 | 16,121 | 2,500 | 6 |
| 100,000 | 1 | 487.4 | 151 | 2 |
| 100,000 | 10 | 1,501 | 346 | 3 |
| 100,000 | 100 | 3,910 | 2,464 | 5 |

结果说明 Assign 路径没有随 backlog 深度线性恶化，主要成本来自 worker 维度和可用队列/索引判断，这符合 typed/indexed scheduler 的优化目标。

Metadata microbenchmark 的结果是混合的：

| 指标 | Legacy | V2 |
|---|---:|---:|
| GetTask | 69.44 ns/op | 27.54 ns/op |
| LeaseComplete | 1,400 ns/op | 4,243 ns/op |
| Heartbeat | 1,487 ns/op | 195 ns/op |
| HeartbeatUnderCompleteP99 heartbeat p99 | 5,505,106 ns | 26,323 ns |
| HeartbeatUnderCompleteP99 ns/op | 10,079 | 7,040 |
| ActiveWorkers | 59,624 ns/op | 67,272 ns/op |
| UpdateWorkflow | 15,336 ns/op | 21,143 ns/op |

V2 明显改善了 heartbeat 路径，尤其是 complete 并发下的 heartbeat p99；GetTask 也更快。但 LeaseComplete、ActiveWorkers 和 UpdateWorkflow 在当前实现中更慢，后续优化应继续关注这些写路径和 view 更新成本。

### Fault Injection

| 故障项 | 结果 |
|---|---|
| worker kill recovery | passed |
| queue redelivery | passed |
| control restart probe | passed |
| logd restart probe | covered by logstore recovery and process logs |

故障恢复测试包括 worker 丢失后的 task redelivery、workflow 已完成 step 不重跑、actor recovery、control 从 shared log bootstrap metadata view 等路径。

### Dashboard Snapshot

Dashboard snapshot 包含：

| 项 | 数量 |
|---|---:|
| tasks | 218 |
| workflows | 3 |
| actors | 2 |
| workers | 3 |
| models | 3 |
| compactable log records | 45 |
| compactable log bytes | 18,283 |

Dashboard 用来查看 materialized view 中的 workflow DAG、task 状态、actor 状态、worker 和 model cache。Compose 实验中 dashboard 至少观测到 3 个 worker，满足多 worker 模拟验收条件。

## 结论

PostgreSQL async materializer 的单机 Compose 对比验收已经通过：内存状态和 shared log 仍是主路径，PostgreSQL 作为可重建 view，写入 QPS 明显下降；dashboard replay consistency 证明 workflow/actor view 与 shared-log replay 最终一致。metadata checkpoint bootstrap 验收也通过了，control restart 可以从 checkpoint + log tail 恢复 metadata view，并明显减少历史 records 读取。

当前 Ubuntu 单机实验的自动汇总结果为 `PASS`。这说明 LogServe 的核心机制能在可复现的单机多进程环境中稳定跑通：

- shared log 是系统状态源，metadata 是可重建视图。
- Workflow 支持 DAG、retry、timeout、replay 和 exactly-once-ish 结果提交。
- Actor 支持 mailbox 串行化、command sequence、snapshot replay、logical trim 和 epoch fencing。
- Typed/indexed scheduler 能避免按 backlog 深度线性扫描，普通 task、target worker task、actor task 和 LLM task 可以通过不同队列/索引调度。
- LLM serving 支持模型注册、mock/vLLM adapter、worker cache 上报、checkpoint cache 和 locality-aware scheduling。
- Benchmark、fault injection、dashboard 和实验报告脚本已经能在单机 Ubuntu 环境复现实验并自动打包结果。

实验边界也需要明确：这组结果验证的是单机机制和回归门禁，不代表生产级多机性能；mock LLM 不能替代真实 GPU/vLLM 延迟；小 checkpoint 只能证明 cache 路径正确，不能反映大模型真实冷启动曲线。下一步更有价值的实验是多节点部署、真实 vLLM/GPU 负载、更大 checkpoint 下的 cold-start 曲线，以及针对 metadata V2 写路径的进一步优化。
