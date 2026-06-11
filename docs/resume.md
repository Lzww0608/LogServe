# LogServe Resume Summary

## 中文项目描述

LogServe 是一个基于 shared log 的 AI runtime 原型，面向 workflow、actor 和 LLM serving 场景。项目实现了 Go 控制面、append-only log 服务、worker 执行器、Python SDK、workflow DAG 调度、有状态 actor 恢复、模型缓存感知调度、checkpoint cache、故障注入、benchmark 和 dashboard。

## 简历条目

- 设计并实现基于 shared log 的 AI workflow runtime：控制面采用 log-first 语义，先写 `TaskSubmitted/Started/Completed`、workflow/actor/LLM 事件，再更新 materialized metadata view，支持从日志 replay 重建状态。
- 实现 Python SDK 与 gRPC client，提供 `@task`、`@workflow`、`@actor`、`llm_generate()` API；修正显式 idempotency 语义，重复 key 且 payload 不一致时返回冲突。
- 实现 workflow DAG runtime：支持 step 依赖解析、ready step 调度、`SCHEDULED/STARTED/SUCCEEDED/FAILED` 状态机、retry、timeout、result ref、失败后恢复，以及 `workflow_id + step_id + input_hash` 级别的 exactly-once-ish 结果去重；worker 侧支持 task/LLM/actor 本地 executor pool。
- 实现有状态 actor runtime：支持 actor 创建、ownership、mailbox 串行化、`ActorCommandSubmitted`、单调 `command_seq`、snapshot replay、logical trim 和 epoch fencing，防止旧 worker 在失联后继续写入 actor 状态。
- 实现 LLM serving 与 locality-aware scheduling：支持 model registry、mock LLM、vLLM OpenAI-compatible adapter、worker model cache 上报、file-backed checkpoint cache、`RESOURCE_ONLY/LOCALITY_AWARE/PREDICTED_LATENCY` 三种调度策略；将 predicted-latency 从调度时扫描 `llm:*` 日志改为基于 `LLMCompleted` 的 materialized EWMA stats。
- 建立系统强化与评估工具：实现 fault injection、workflow/task/actor/LLM benchmark、snapshot/locality/replay ablation、dashboard snapshot、backpressure 和单机 Ubuntu 实验脚本。

## 实验结果写法

在 Ubuntu 22.04 单机、3 worker、mock LLM 环境下完成端到端实验：

- `go test ./...`、`go vet`、race test、Python unittest、Python compile、gRPC dependency check 全部通过。
- Workflow p95/p99 latency 为 823 ms；task throughput 为 5.17 tasks/s，task p99 latency 为 207 ms。
- Actor snapshot replay 从 full replay 的 21 条 command 降至 1 条 command，无 snapshot baseline 为 21 条。
- Snapshot-aware retention 通过 logical trim 标记 actor stream 中可压缩日志，并在 dashboard/benchmark 中暴露 compactable records/bytes。
- Locality-aware scheduler 将 cache hit rate 从 resource-only 的 0.833 提升到 1.000，p95 latency 从 305 ms 降至 205 ms，cold start rate 从 0.167 降至 0。
- File-backed checkpoint cache 验证通过：cold request `cache_hit=false`、`checkpoint_fetch_ms=1`，warm request `cache_hit=true`、`checkpoint_fetch_ms=0`，并在 worker-local cache 中生成 `model-D-v1.checkpoint` artifact。
- Shared log benchmark 中，20,000 条记录、16 streams、256-byte payload 下，batch/interval fsync append throughput 达到约 239k/266k records/s，显著高于 always fsync 的约 1.7k records/s。
- Fault injection 验证 worker kill recovery、queue redelivery、control restart probe 均通过；dashboard snapshot 展示 task、workflow、actor、worker 和 model cache 当前状态。

## 英文简历版本

- Built LogServe, a shared-log-based AI runtime prototype in Go and Python, covering workflow DAG execution, stateful actors, LLM serving, model-cache-aware scheduling, replay, checkpointing, fault injection, benchmarking, and dashboard snapshots.
- Implemented log-first control-plane semantics where task/workflow/actor/LLM events are appended before metadata mutation, enabling recovery and consistency checks by replaying shared-log streams.
- Designed a Python SDK with native gRPC transport and `@task`, `@workflow`, `@actor`, and LLM APIs; added explicit idempotency semantics with conflict detection for reused keys and mismatched payloads.
- Implemented workflow recovery with DAG scheduling, retry/timeout handling, result references, replay validation, exactly-once-ish step result deduplication based on `workflow_id + step_id + input_hash`, and worker-local executor pools for task, LLM, and actor execution.
- Implemented an actor runtime with serialized mailboxes, `command_seq`, snapshots, snapshot-aware logical trim, ownership transfer, and epoch fencing to prevent stale workers from applying actor state changes.
- Added model registry, mock/vLLM LLM adapters, worker model-cache reporting, file-backed checkpoint cache, and resource-only/locality-aware/predicted-latency scheduling backed by materialized EWMA latency stats instead of replay-all scans on the hot path.
- Evaluated on a single-node Ubuntu setup with 3 workers: locality-aware scheduling improved cache hit rate from 0.833 to 1.000 and reduced p95 latency from 305 ms to 205 ms; actor snapshot replay reduced replay work from 21 commands to 1; checkpoint cache cold/warm probe produced a persisted worker-local `model-D-v1.checkpoint` artifact.

## 边界说明

- 当前实验是单机 Ubuntu 环境，不等同于多机生产部署。
- LLM 结果主要来自 mock serving；vLLM adapter 已实现，但未在 GPU 负载下给出性能结论。
- 系统语义是 exactly-once-ish，不宣称严格 distributed exactly-once。
- Log retention 当前是 logical trim，不物理删除 segment；compactable bytes 用于衡量后续 compaction 空间。
- Kubernetes 部署文件用于展示云原生形态，核心实验仍以单机脚本结果为准。
