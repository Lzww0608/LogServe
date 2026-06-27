# LogServe 简历和面试表述

## 中文项目描述

LogServe 是一个基于 shared log 的 AI runtime 原型，覆盖 task、workflow、actor 和 LLM serving。项目用 Go 实现 logd、control plane 和 worker，用 Python SDK 暴露 `@task`、`@workflow`、`@actor` 和 `llm_generate()`。核心思路是先把状态变化写入 append-only log，再从日志 materialize 当前视图，从而支持 replay、恢复、故障注入和可验证优化。

## 简历条目

- 设计并实现基于 shared log 的 AI workflow runtime：控制面采用 log-first 语义，先写 task、workflow、actor、LLM 事件，再更新 materialized metadata view，支持服务重启后从日志 replay 重建状态。
- 实现 Python SDK 和 gRPC client，提供 `@task`、`@workflow`、`@actor`、`llm_generate()` API；补齐显式 idempotency 语义，同一个 key 搭配不同 payload 会返回冲突。
- 实现 workflow DAG runtime：支持 step 依赖解析、ready queue 调度、retry、timeout、result ref、失败恢复，以及 `workflow_id + step_id + input_hash` 级别的 exactly-once-ish 结果去重。
- 实现 stateful actor runtime：支持 actor ownership、mailbox 串行化、单调 `command_seq`、snapshot replay、logical trim 和 epoch fencing，防止旧 worker 在失联后继续写入 actor 状态。
- 实现 LLM serving 与 cache-aware scheduling：支持 model registry、mock LLM、vLLM OpenAI-compatible adapter、worker model cache 上报、file-backed checkpoint cache，以及 `RESOURCE_ONLY`、`LOCALITY_AWARE`、`PREDICTED_LATENCY` 三种策略。
- 完成多项运行时优化和验收：scheduler v2、worker batch poll/long-poll、PostgreSQL async materializer、metadata checkpoint、physical compaction、CRC32C checksum、msgpack executor 和 dashboard replay consistency。

## 实验结果写法

最新单机 Ubuntu 顶层验收在 `reports/ubuntu-project-accepted` 通过，结果为 `PASS`。

- `go test ./...`、`go vet`、logstore/control/metadata/worker race tests、Python SDK tests 和 Python compileall 全部通过。
- Docker Compose 端到端实验通过，环境包含 PostgreSQL、MinIO、logd、control 和 3 个 worker。
- Workflow p95/p99 latency 为 924 ms；task throughput 为 5.110 tasks/s；task p99 latency 为 209 ms。
- Actor snapshot replay 从 21 条 command 降到 1 条 command；dashboard 报告 45 条 compactable actor-log records 和 18,382 compactable bytes。
- Metadata checkpoint 验收中，full replay 读取 614 条 records，checkpoint-plus-tail replay 读取 71 条，且 156 个对象一致性检查通过。
- PostgreSQL async materializer 将 transaction rate 从 72.382 tx/s 降到 1.304 tx/s，将 row writes rate 从 100.519 rows/s 降到 16.57 rows/s，同时 task throughput 和 p99 未退化。
- File-backed checkpoint cache 验证通过：cold request `cache_hit=false`、`checkpoint_fetch_ms=1`，warm request `cache_hit=true`、`checkpoint_fetch_ms=0`。

## 英文简历表述

- Built LogServe, a shared-log-based AI runtime in Go and Python for task execution, workflow DAGs, stateful actors, LLM serving, replay, checkpointing, fault injection, benchmarking, and dashboard snapshots.
- Implemented log-first control-plane semantics where task, workflow, actor, and LLM events are appended before metadata mutation, enabling restart recovery and consistency checks from shared-log replay.
- Designed a Python SDK with native gRPC transport and `@task`, `@workflow`, `@actor`, and LLM APIs, including explicit idempotency conflict detection for reused keys with mismatched payloads.
- Implemented workflow DAG recovery with ready-step scheduling, retry/timeout handling, result references, replay validation, and exactly-once-ish step result deduplication using `workflow_id + step_id + input_hash`.
- Implemented a stateful actor runtime with serialized mailboxes, `command_seq`, snapshots, snapshot-aware logical trim, ownership transfer, and epoch fencing to reject stale worker completions.
- Added model registry, mock/vLLM LLM adapters, worker model-cache reporting, file-backed checkpoint cache, and resource-only/locality-aware/predicted-latency scheduling backed by materialized EWMA latency stats.
- Validated the system on a single-node Ubuntu Compose setup: actor snapshot replay reduced replay work from 21 commands to 1, metadata checkpoint replay read 71 records versus 614 for full replay, and PostgreSQL async materialization reduced database transaction/write rates without task-path regression.

## 面试边界

- 当前结果来自单机 Ubuntu 多进程环境，不等同于多机生产部署。
- LLM 性能主要来自 mock LLM；vLLM adapter 已实现，但没有给出真实 GPU 负载性能结论。
- 系统语义是 exactly-once-ish，不宣称严格 distributed exactly-once。
- checkpoint cache 使用小文件验证路径正确，不能代表大模型真实冷启动曲线。
- Kubernetes 文件是部署示例，当前实验结论以单机脚本和 Compose 验收为准。

## 面试时可以突出的点

1. 为什么用 shared log：它让状态变化可追溯，重启后可以 replay，metadata view 不再是唯一事实来源。
2. 为什么说 exactly-once-ish：worker 可能重复执行，但控制面对 step result 和 actor command 做幂等应用。
3. actor 的难点在哪里：同一 actor 要按 mailbox 顺序执行，还要防止旧 owner worker 的完成写回。
4. LLM 调度的价值：不是替代 vLLM，而是让模型缓存、checkpoint cold start 和 worker placement 进入统一调度。
5. 优化如何证明有效：每项优化都对应 benchmark、fault injection、dashboard snapshot 或 Ubuntu 验收结果。