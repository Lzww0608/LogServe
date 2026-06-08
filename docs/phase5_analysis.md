# Phase 5 System Analysis

Phase 5 turns LogServe from a feature-complete prototype into an analyzable
system. The focus is not only "can it run", but whether failures, scheduling
choices, replay, snapshots, and backpressure have observable consequences.

## Experiment Environment

The intended single-machine experiment environment is:

```text
Linux lab2439 6.8.0-111-generic #111~22.04.1-Ubuntu SMP PREEMPT_DYNAMIC Tue Apr 14 17:13:45 UTC x86_64 GNU/Linux
```

This is a single-node Ubuntu 22.04-class Linux host. The default scripts run
logd, control, and multiple workers as local processes. Docker Compose can be
used on the same host to add MinIO for S3-compatible result storage.

## Fault Injection

Run:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_fault_injection.ps1
```

The script executes the automated recovery tests for:

- worker loss during workflow execution
- actor owner loss and recovery through replay/snapshot
- running task redelivery after the lease expires
- poll-before-start task redelivery after worker loss
- control restart bootstrap for workflow, actor, model, and backpressure streams

It also probes control and logd process restarts. On control startup, LogServe
scans shared-log streams and rebuilds materialized workflow, actor, model, and
backpressure state. Running workflow steps are restored to the in-memory queue
from their logged DAG definitions. Plain ad-hoc tasks do not yet have enough
function-spec metadata in the shared log to be automatically resumed after a
control restart, so they remain a prototype limitation.

## Benchmarks

Run:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_benchmark.ps1
```

The benchmark report includes:

- workflow latency for RAG plus mock LLM
- task throughput for simple Python tasks
- actor replay cost with snapshots versus effectively disabled snapshots
- LLM cold start and warm cache latency
- locality-aware scheduling compared with resource-only scheduling

The report is written to `benchmarks/phase5_latest.json`.

## Ablations

The benchmark harness records three ablation views:

- `locality_ablation`: compares `RESOURCE_ONLY` and `LOCALITY_AWARE` policies
  using model-A requests and worker model caches.
- `actor_recovery_snapshot_ablation`: compares replay command counts when
  snapshots are frequent versus when snapshot frequency is set high enough to
  act as disabled for the run.
- `replay_ablation`: documents the replay-off baseline as an analysis-only
  state. Without replay, the system can still execute tasks, but cannot
  independently reconstruct state after failures.

## Dashboard

Run against a live control plane:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_dashboard.ps1
```

This writes `dashboard/snapshot.json`. Open `dashboard/index.html` to inspect:

- queue depth and backpressure configuration
- workflow DAG and step states
- task table
- actor table
- worker capacity and model cache

## Backpressure

Backpressure rejects new non-duplicate task submissions when the in-memory queue
length reaches the configured high watermark. It also rejects new submissions
when the most recent control-plane log append latency is above the configured
slow threshold. Idempotent duplicate submissions bypass backpressure and return
the existing task id. Running tasks that exceed the redelivery timeout, including
tasks leased by `PollTask` before `StartTask`, are moved back to queued state
and can be polled by another worker.

Configure through:

```powershell
'{"queue_high_watermark":128,"redelivery_timeout_ms":30000,"log_append_slow_ms":250}' |
  go run ./cmd/logservectl backpressure-set
```

## Result Store

The default result store is filesystem-backed `local://` storage. For the
single-machine Compose experiment, control is configured to use MinIO through
the S3-compatible adapter:

```text
LOGSERVE_RESULT_STORE=minio
LOGSERVE_S3_ENDPOINT=http://minio:9000
LOGSERVE_S3_BUCKET=logserve-results
LOGSERVE_S3_ACCESS_KEY=logserve
LOGSERVE_S3_SECRET_KEY=logserve123
```

Large workflow results and actor snapshots are written to this store, while log
events retain only `result_ref` or `snapshot_ref`.

## Kubernetes

Kubernetes manifests are under `deployments/k8s`. They deploy logd, control, and
two workers with different model caches. The provided script builds a local
image and applies the manifests:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_kind_deploy.ps1
```

The manifests are intentionally small and suitable for kind or minikube demos.
