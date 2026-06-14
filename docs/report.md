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
- Scheduler：实现 `RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 三种策略。`PREDICTED_LATENCY` 使用由 `LLMCompleted` 事件增量维护的 materialized stats，按 `(model_name, model_version, worker_id)` 记录 request count、cache-hit count、EWMA total/model-load/checkpoint-fetch latency 和 last update time，调度时只做 `O(number_of_workers)` 查询，不在热路径扫描 `llm:*` streams。

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

实验在用户单机 Ubuntu 环境完成：

```text
Linux lab2439 6.8.0-111-generic x86_64 GNU/Linux
Ubuntu 22.04 single-node environment
Project path: /home/lab2439/Work/lzww/LogServe
Mode: single node, 3 workers, mock LLM, file-backed checkpoint cache
```

这组实验不用于说明多机生产性能，重点检查这些机制是否能在单机环境跑通：log-first、replay、redelivery、actor recovery、snapshot、locality scheduling、checkpoint cache 和 dashboard materialization。

## 验证结果

### 基础验证

最新实验运行目录：

```text
reports/experiment-20260610T013044794327660Z
```

主要验证命令均通过：

| 验证项 | 结果 |
|---|---:|
| `go test ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -race ./internal/control ./internal/worker` | PASS |
| Python unittest | PASS |
| Python compileall | PASS |
| gRPC dependency check | PASS |
| logstore benchmark | PASS |
| fault injection tests | PASS |
| runtime logd/control/workers start | PASS |
| Benchmark | PASS |
| checkpoint cache probe | PASS |
| checkpoint cache artifact check | PASS |
| dashboard snapshot | PASS |

### Benchmark

本次单机实验结果如下：

| 指标 | 结果 |
|---|---:|
| Workflow p95 latency | 823 ms |
| Workflow p99 latency | 823 ms |
| Task throughput | 5.17 tasks/s |
| Task p99 latency | 207 ms |
| Actor full replay commands | 21 |
| Actor snapshot replay commands | 1 |
| Actor trimmed replay commands | 1 |
| Actor no-snapshot replay commands | 21 |
| LLM cold total latency | 98 ms |
| LLM warm total latency | 17 ms |

Actor snapshot ablation 显示 snapshot 生效：同样 20 次 command 后，snapshot replay 和 trimmed replay 只需回放 1 条 command，而无 snapshot 需要回放 21 条。Dashboard 和 benchmark 会报告 compactable log records/bytes，用于衡量 snapshot-aware retention 可以释放的日志空间。

### Locality Scheduling Ablation

| 策略 | Cache hit rate | Cold start rate | p95 latency | SLO violation |
|---|---:|---:|---:|---:|
| Resource-only | 0.833 | 0.167 | 305 ms | 0.167 |
| Locality-aware | 1.000 | 0.000 | 205 ms | 0.000 |
| Predicted-latency | 1.000 | 0.000 | 205 ms | 0.000 |

结果说明：在 3 worker、模型缓存不均匀的设置下，locality-aware 和 predicted-latency 避免了额外 cold start，cache hit rate 更高，p95 latency 更低。

### Checkpoint Cache Probe

最新 checkpoint cache 实验结果：

| 指标 | Cold | Warm |
|---|---:|---:|
| cache hit | false | true |
| checkpoint fetch | 1 ms | 0 ms |
| cache used | 3,145,728 bytes | 3,145,728 bytes |
| cache capacity | 16,777,216 bytes | 16,777,216 bytes |
| model load | 1 ms | 1 ms |
| first token | 15 ms | 15 ms |
| total latency | 18 ms | 18 ms |
| worker | bench-worker-1 | bench-worker-1 |

Artifact check 也通过：

```text
runtime/model-cache/worker-1/model-D-v1.checkpoint
runtime/model-cache/worker-1/model-D-v1.checkpoint.manifest.json
```

这说明 file-backed checkpoint cache 写到了 worker-local cache，而不是只创建 source checkpoint。由于 checkpoint 文件默认 1 MiB，fetch/load 延迟较小；这组数据主要用于确认功能路径。

### Logstore Benchmark

20,000 records、16 streams、256-byte payload 的单机结果：

| fsync policy | Append records/s | Read records/s | Recover ms | Segments |
|---|---:|---:|---:|---:|
| always | 1,685.74 | 529,000.62 | 63 | 7 |
| batch | 239,518.24 | 825,535.97 | 80 | 7 |
| interval | 266,441.10 | 738,468.94 | 63 | 7 |

结果和写入策略一致：`always` 强同步写入最慢；`batch` 和 `interval` 的 append throughput 明显更高。恢复时间保持在几十毫秒量级。

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
| tasks | 82 |
| workflows | 3 |
| actors | 2 |
| workers | 3 |
| models | 2 |
| compactable log bytes | dashboard snapshot reports |

Dashboard 用来查看 materialized view 中的 workflow DAG、task 状态、actor 状态、worker 和 model cache。

## 结论

LogServe 现在具备以下能力：

- shared log 是系统状态源，metadata 是可重建视图。
- Workflow 支持 DAG、retry、timeout、replay 和 exactly-once-ish 结果提交。
- Actor 支持 mailbox 串行化、command sequence、snapshot replay 和 epoch fencing。
- Shared log 支持 per-stream logical trim 和 compactable bytes 统计，actor snapshot 后可从 snapshot + tail log replay。
- LLM serving 支持模型注册、mock/vLLM adapter、worker cache 上报、checkpoint cache 和 locality-aware scheduling。
- Benchmark、fault injection、dashboard 和实验报告脚本已经能在单机 Ubuntu 环境复现实验。

这些结果说明当前机制能在单机实验中稳定跑通，并给出几个对比基线；它们不代表生产级多机性能。下一步可以补多节点部署实验、真实 vLLM/GPU 负载、持久化 PostgreSQL/MinIO 端到端压测，以及更大 checkpoint 下的 cold-start 曲线。
