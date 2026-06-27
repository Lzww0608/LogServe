# LogServe 实验报告

## 结论

当前最新的完整验收是 `reports/ubuntu-project-accepted`，结果为 `PASS`。这轮验收在单台 Ubuntu 服务器上运行，使用 Docker Compose 启动 PostgreSQL、MinIO、logd、control 和 3 个 worker，并嵌套执行 Compose 端到端实验、metadata checkpoint 验收和 PostgreSQL async materializer 验收。

这组结果可以证明：LogServe 的核心机制在单机多进程环境中可复现，主要回归门禁通过，shared log、workflow、actor、LLM serving、checkpoint、physical compaction 和 async materializer 的关键路径没有被破坏。

边界也必须说清楚：这不是生产级多机性能结论，不是真实 GPU/vLLM 压测，也不能推断大模型 checkpoint 的真实冷启动曲线。

## 验收环境

```text
Project path: /home/lab2439/Work/lzww/LogServe
Run directory: reports/ubuntu-project-accepted
Package: reports/ubuntu-project-accepted/ubuntu-project-acceptance-package.tar.gz
Runtime: Docker Compose, PostgreSQL, MinIO, logd, control, 3 workers
LLM: mock LLM, worker-local file-backed checkpoint cache
Scheduler: LOGSERVE_SCHEDULER_V2=1
Verdict: PASS
```

## 顶层验收项

| Check | Result |
|---|---:|
| `go_baseline_tests` | pass |
| `physical_compaction_tests` | pass |
| `logstore_race_tests` | pass |
| `python_script_tests` | pass |
| `python_compileall` | pass |
| `compose_experiment_pass` | pass |
| `checkpoint_acceptance_pass` | pass |
| `postgres_async_acceptance_pass` | pass |

主要命令也全部通过：

| Command | Status | Seconds |
|---|---:|---:|
| `go_test_all` | PASS | 27 |
| `go_vet` | PASS | 0 |
| `go_test_physical_compaction` | PASS | 1 |
| `go_race_logstore` | PASS | 1 |
| `go_race_core` | PASS | 3 |
| `python_script_tests` | PASS | 0 |
| `python_sdk_tests` | PASS | 0 |
| `python_compileall` | PASS | 0 |
| `compose_experiment` | PASS | 80 |
| `checkpoint_acceptance` | PASS | 2 |
| `postgres_async_acceptance` | PASS | 116 |

## Compose 端到端实验

Compose 子实验结果为 `PASS`，路径为：

```text
reports/ubuntu-project-accepted/compose_experiment
```

通过的关键检查：

| Check | Result |
|---|---:|
| all recorded commands pass | PASS |
| relaxed fsync faster than always | PASS |
| locality cache hit not worse than resource-only | PASS |
| warm checkpoint request hit cache | PASS |
| actor snapshot replay less than full replay | PASS |
| dashboard has at least three workers | PASS |
| dashboard replay consistent | PASS |

核心结果：

| Metric | Result |
|---|---:|
| Workflow p95 / p99 latency | 924 ms / 924 ms |
| Task throughput | 5.110 tasks/s |
| Task p99 latency | 209 ms |
| Actor full replay commands | 21 |
| Actor no-snapshot replay commands | 21 |
| Actor snapshot replay commands | 1 |
| Actor trimmed replay commands | 1 |
| Actor compactable log records | 45 |
| Actor compactable log bytes | 18,382 |
| LLM cold / warm total latency | 111 ms / 17 ms |
| LLM cold / warm checkpoint fetch | 1 ms / 0 ms |
| Dashboard workers | 3 |
| Dashboard replay consistency | true |

这里最重要的不是绝对延迟，而是机制是否成立。actor snapshot 把 replay work 从 21 条 command 降到 1 条；checkpoint cache 的 warm request 命中本地缓存；dashboard 当前视图和 replay 结果一致。

## Logstore benchmark

这轮 logstore benchmark 使用 20,000 records、16 streams、256-byte payload。

| fsync policy | Append records/s | Read records/s | Recover ms | Segments |
|---|---:|---:|---:|---:|
| always | 1,760.664 | 656,928.533 | 36 | 7 |
| batch | 293,717.448 | 906,235.115 | 36 | 7 |
| interval | 296,456.268 | 999,045.612 | 31 | 7 |

结论：`always` 强同步写入最慢；`batch` 和 `interval` 的 append throughput 明显更高。恢复时间在这次 workload 下仍是几十毫秒量级。

## Scheduler benchmark

Scheduler mixed backlog benchmark 覆盖 `queue_depth = 1k/10k/100k` 和 `workers = 1/10/100`。

| Queue depth | Workers | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|---:|
| 1,000 | 1 | 554.3 | 105 | 2 |
| 1,000 | 10 | 2,003 | 367 | 4 |
| 1,000 | 100 | 17,887 | 2,480 | 6 |
| 10,000 | 1 | 476.9 | 128 | 2 |
| 10,000 | 10 | 1,795 | 345 | 3 |
| 10,000 | 100 | 15,502 | 2,497 | 6 |
| 100,000 | 1 | 451.6 | 151 | 2 |
| 100,000 | 10 | 1,481 | 345 | 3 |
| 100,000 | 100 | 4,078 | 2,468 | 5 |

结果说明 Assign 路径没有跟 backlog 深度一起线性恶化。主要成本来自 worker 维度和队列/索引判断，这符合 typed/indexed scheduler 的设计目标。

## Metadata benchmark

Metadata benchmark 的结果是混合的。

| Metric | Legacy | V2 |
|---|---:|---:|
| GetTask | 66.30 ns/op | 28.44 ns/op |
| LeaseComplete | 1,715 ns/op | 3,767 ns/op |
| Heartbeat | 1,526 ns/op | 169.1 ns/op |
| HeartbeatUnderCompleteP99 heartbeat p99 | 5,382,126 ns | 17,242 ns |
| ActiveWorkers | 59,066 ns/op | 71,933 ns/op |
| UpdateWorkflow | 12,664 ns/op | 20,912 ns/op |

V2 明显改善了 heartbeat 和 GetTask，尤其是 complete 并发下的 heartbeat p99。LeaseComplete、ActiveWorkers 和 UpdateWorkflow 在当前实现中更慢，后续优化应继续看这些写路径和 view 更新成本。

## Metadata checkpoint 验收

Checkpoint 子验收路径为：

```text
reports/ubuntu-project-accepted/checkpoint_acceptance/checkpoint_acceptance
```

Workload：

| Item | Count |
|---|---:|
| Tasks | 120 |
| Workflows | 12 |
| Actors | 12 |
| LLM streams | 40 |
| Tail events | 68 |

Replay work：

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration ms | 6.463 | 5.506 | 0.8519 |

checkpoint payload 覆盖 196 个 stream、132 个 task、12 个 workflow、12 个 actor 和 2 条 LLM stats entry。一致性检查结果为 `consistent=true`，检查对象数为 156。

通过的检查：

| Check | Result |
|---|---:|
| `checkpoint_created` | pass |
| `checkpoint_read_records_reduced` | pass |
| `checkpoint_replay_consistent` | pass |
| `checkpoint_retention` | pass |
| `checkpoint_tail_only_reads` | pass |
| `corrupt_checkpoint_fallback` | pass |

正确表述：metadata checkpoint 把 control restart 从全量历史扫描改成 checkpoint + tail replay，历史 records 读取量明显下降。不要说它消除了 stream 访问，因为每个 stream tail 仍要检查。

## PostgreSQL async materializer 验收

PostgreSQL async 子验收路径为：

```text
reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare
```

结果：

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput | 5.0 tps | 5.0 tps | 1.0 |
| Task submit p99 | 209 ms | 207 ms | 0.9904 |
| PostgreSQL tx/s | 72.382 | 1.304 | 0.018 |
| PostgreSQL row writes/s | 100.519 | 16.57 | 0.1648 |

通过的检查：

| Check | Result |
|---|---:|
| `task_throughput_within_tolerance` | pass |
| `task_submit_p99_within_tolerance` | pass |
| `postgres_transactions_per_sec_reduced` | pass |
| `postgres_row_writes_per_sec_reduced` | pass |
| `async_materializer_mode_observed` | pass |
| `async_materializer_flush_errors_zero` | pass |

async dashboard 中观察到 `materializer_mode=async`、`pending_deltas=6`、`flush_errors=0`、`eventual_lag_ms=746`。

正确表述：async materializer 大幅降低 PostgreSQL 写入压力，任务吞吐和 p99 在验收阈值内没有退化。这一轮不能说吞吐显著提升。

## Fault injection 和 dashboard

Fault injection 覆盖：

| Fault | Result |
|---|---|
| worker kill recovery | passed |
| queue redelivery | passed |
| control restart probe | passed |
| logd restart probe | covered by logstore recovery and process logs |

Dashboard snapshot：

| Item | Count |
|---|---:|
| tasks | 84 |
| workflows | 3 |
| actors | 2 |
| workers | 3 |
| models | 3 |
| compactable log records | 45 |
| compactable log bytes | 18,382 |

Dashboard replay consistency 为 `true`，检查了 3 个 workflow 和 2 个 actor，没有失败项。

## 总结

当前可以稳妥说明三点：

1. 核心机制已经跑通：shared log、workflow replay、actor mailbox/epoch fencing、LLM cache-aware scheduling、checkpoint cache、dashboard materialization 都在单机多进程环境中通过验收。
2. 优化有证据：scheduler v2 不随 backlog 深度线性恶化；metadata checkpoint 减少历史 records 读取；PostgreSQL async materializer 降低数据库写入压力；actor snapshot 把 replay command 数降到 1。
3. 边界清楚：还没有多节点生产压测、真实 GPU/vLLM 压测和大 checkpoint 冷启动曲线。