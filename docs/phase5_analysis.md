# Phase 5 System Analysis

Phase 5 turns LogServe from a feature-complete prototype into an analyzable
system. The focus is not only "can it run", but whether failures, scheduling
choices, replay, snapshots, and backpressure have observable consequences.

## Fault Injection

Run:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_fault_injection.ps1
```

The script executes the automated recovery tests for:

- worker loss during workflow execution
- actor owner loss and recovery through replay/snapshot
- running task redelivery after the lease expires

It also probes control and logd process restarts. Current limitation: control
metadata is still in-memory, so a restarted control plane can replay named
workflow, actor, and LLM streams on request, but it does not yet scan every log
stream and automatically rebuild the full metadata index.

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

Backpressure currently rejects new task submissions when the in-memory queue
length reaches the configured high watermark. Running tasks that exceed the
redelivery timeout are moved back to queued state and can be polled by another
worker.

Configure through:

```powershell
'{"queue_high_watermark":128,"redelivery_timeout_ms":30000}' |
  go run ./cmd/logservectl backpressure-set
```

## Kubernetes

Kubernetes manifests are under `deployments/k8s`. They deploy logd, control, and
two workers with different model caches. The provided script builds a local
image and applies the manifests:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_kind_deploy.ps1
```

The manifests are intentionally small and suitable for kind or minikube demos.
