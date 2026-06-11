# LogServe

LogServe is a lightweight shared-log-based runtime for AI workflow
infrastructure. It is built around a log-first control plane, Python SDK,
workers, workflow DAG runtime, stateful actors, and LLM serving with
model-cache-aware scheduling.

The project covers:

1. Distributed task execution through `@task`, shared-log task events, and
   worker polling.
2. Multi-step workflow execution with DAG scheduling, replay, retry, timeout,
   idempotency, and result references.
3. Stateful Python actors with mailbox serialization, command sequencing,
   snapshot replay, ownership transfer, and epoch fencing.
4. LLM serving with model registry, mock/vLLM adapters, worker model-cache
   reporting, file-backed checkpoint cache, and locality-aware scheduling.
5. Phase 5 hardening with fault injection, benchmark scripts, ablation studies,
   dashboard snapshots, backpressure, and Kubernetes manifests.

## Repository Layout

```text
cmd/
  logserve-logd      Shared log service
  logserve-control   Control plane and in-memory queue
  logserve-worker    Worker agent and Python executor bridge
  logserve-dev       Single-process local dev runner
  logservectl        CLI fallback for the Python SDK
proto/               gRPC contracts
internal/logstore    Segmented append-only log v1
internal/control     Task API and status materialization
internal/workflow    Workflow DAG model, argument resolution, replay
internal/actor       Actor state model and replay reducer
internal/worker      Worker polling and task execution
internal/objectstore Local result store v0 for large workflow results
sdk/python/logserve  Python SDK
executor/python      Python function executor
deployments/         Docker Compose and Kubernetes manifests
docs/report.md       Project report and experiment writeup
docs/resume.md       Resume-ready project summary
```

## Local Demo Without Docker

Docker is optional for the Phase 1 local loop. From the repository root:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase1_smoke.ps1
```

Or run the services manually:

```powershell
go run ./cmd/logserve-dev
```

In another PowerShell:

```powershell
$env:PYTHONPATH = "$PWD\sdk\python"
python .\examples\hello_task\add.py
```

Expected output:

```text
3
```

## Phase 2 Workflow Demo

Run the Python `@workflow` DSL example:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase2_smoke.ps1
```

Expected output:

```text
answer:hello:doc:vec:hello
```

The example shape is:

```python
@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    ans = generate_mock(query, docs)
    return ans
```

During submission the Python SDK traces `@task` calls into a DAG. The Go control
plane schedules only ready steps, substitutes completed step outputs into
dependent step inputs, and writes workflow events to `wf:<workflow_id>`.

## Separate Process Mode

```powershell
go run ./cmd/logserve-logd --addr 127.0.0.1:50051 --data-dir data/logstore --segment-size-bytes 67108864 --fsync-policy always
go run ./cmd/logserve-control --addr 127.0.0.1:50052 --log-addr 127.0.0.1:50051
go run ./cmd/logserve-worker --worker-id worker-1 --control-addr 127.0.0.1:50052 --log-addr 127.0.0.1:50051
```

Then run the same Python demo.

## Worker Local Executor Pool

Workers dispatch polled tasks into local executor queues before completing them
back to the control plane:

```text
PollTask -> local queue -> executor goroutine pool -> CompleteTask
```

Use these flags to size local execution independently by task type:

```text
--capacity 4
--task-pool-size 4
--llm-pool-size 4
--actor-pool-size 2
```

If a pool size is `0` or omitted, it follows `--capacity` for backward
compatibility. Ordinary Python tasks and LLM requests can run concurrently.
Actor work is dispatched through an actor pool, but each `actor_id` is still
protected by a per-actor lock and the control-plane mailbox/`command_seq`
rules, so methods for the same actor remain ordered.

Each `TaskStarted` event includes `local_queue_wait_ms`, which records how long
the task waited in the worker-local queue before an executor goroutine started
it. This is the worker-side signal for queue wait and pool saturation.

## Python SDK Transport And Idempotency

The Python SDK defaults to `LOGSERVE_SDK_TRANSPORT=auto`: it uses the native
gRPC transport when `grpcio` and `protobuf` are installed, and falls back to
`logservectl` when those packages are unavailable. Force one mode with:

```powershell
$env:LOGSERVE_SDK_TRANSPORT = "grpc" # require native gRPC
$env:LOGSERVE_SDK_TRANSPORT = "cli"  # force CLI fallback
```

Install the SDK gRPC dependencies on the experiment machine with:

```powershell
python -m pip install -r .\sdk\python\requirements.txt
```

SDK calls are non-idempotent by default. Repeating the same function and
arguments creates a new submission unless the caller explicitly passes
`idempotency_key`. If a key is reused with a different request payload, the
control plane rejects it as an idempotency conflict instead of silently
returning the first result.

## Shared Log v1 Benchmark

The shared log uses rolling segment files, a rebuilt-on-start index, and an
index-backed `ReadLog` path that reads payloads from segment files instead of
keeping every record body in memory. `logd` exposes:

```text
--segment-size-bytes
--fsync-policy always|batch|interval
--fsync-interval-ms
```

On the Ubuntu single-node experiment machine, run:

```bash
bash scripts/logstore_v1_benchmark.sh
```

The script writes a generated JSON report to
`benchmarks/logstore_v1_latest.json` and compares append, read, recovery time,
and segment count across `always`, `batch`, and `interval` fsync policies. You
can tune the workload through environment variables such as
`LOGSERVE_LOGBENCH_RECORDS`, `LOGSERVE_LOGBENCH_PAYLOAD_BYTES`, and
`LOGSERVE_LOGBENCH_SEGMENT_SIZE_BYTES`.

## Docker Compose

The Compose file starts PostgreSQL, NATS JetStream, MinIO, logd, control, and a worker:

```powershell
docker compose -f deployments/docker-compose.yml up --build
```

The default single-process development path uses the in-memory queue and a
materialized metadata view. Control startup bootstraps workflow, actor, model,
plain ad-hoc task specs, and backpressure state from the shared log. PostgreSQL
migrations are included under `internal/metadata/migrations`; Compose mode runs
the control plane with `LOGSERVE_METADATA_STORE=postgres` and writes the
materialized dashboard/task/workflow/actor/model view to PostgreSQL. If the
PostgreSQL tables are dropped, restart control after logd and `BootstrapFromLog`
will recreate the tables and rebuild the view from shared log streams.

## Tests

```powershell
go test ./...
```

Covered Phase 1 checks:

- append/read and idempotent append in shared log
- recovery truncation for partial log tail
- worker heartbeat, task execution, status query, and task event log chain

Covered Phase 2 checks:

- `simple_rag` workflow completion
- worker stops after `embed`; a restarted worker continues from `search` without re-running `embed`
- replayed workflow state from shared log matches metadata state
- duplicate step completion does not write a second workflow final result
- failed steps retry according to `max_attempts`
- timed-out steps fail and retry according to `max_attempts`
- poll-before-start worker loss redelivers the leased task
- ordinary ad-hoc task specs are restored from `TaskSubmitted` after control restart
- stale task completions are rejected by task lease epoch

## Phase 2 Semantics

Workflow source of truth is the shared log. Metadata is a materialized current
view and can be checked against replay through `ReplayWorkflow`.

LogServe Phase 2 provides exactly-once-ish workflow step results, not strict
distributed exactly-once execution. The worker may execute a task at least once.
The workflow engine deduplicates final step results using:

```text
workflow_id + step_id + input_hash
```

Retry attempts add an attempt number to the task dispatch key so a failed
attempt can run again, while duplicate successful completions for the same
step/input do not create another workflow final result.

Large workflow step results are written through the result-store interface and
workflow log events keep only `result_ref`. The default local adapter stores
objects under a filesystem-backed `local://` namespace. Set
`LOGSERVE_RESULT_STORE=minio` or `LOGSERVE_RESULT_STORE=s3` with
`LOGSERVE_S3_ENDPOINT`, `LOGSERVE_S3_BUCKET`, `LOGSERVE_S3_ACCESS_KEY`, and
`LOGSERVE_S3_SECRET_KEY` to use the S3-compatible MinIO adapter. The Compose
environment wires this to its MinIO service.

## Phase 3 Actor Demo

Run the Python `@actor` counter example:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase3_smoke.ps1
```

Expected output:

```text
100
```

The example shape is:

```python
@actor
class Counter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value
```

The SDK creates an actor instance with `create_actor(Counter)`. Calls such as
`counter.inc()` are submitted to the control plane as actor tasks targeted at
the current owner worker.

Covered Phase 3 checks:

- `Counter` actor recovery after the first worker exits at 100 `inc()` calls
- a second worker takes ownership and `get()` returns `100`
- replayed actor state from `actor:<actor_id>` matches metadata state
- snapshots reduce replay work compared with full command replay
- 1000 concurrent `inc()` submissions serialize through the actor mailbox and
  final `get()` returns `1000`
- stale actor completions are rejected by worker id plus epoch fencing

## Phase 3 Semantics

Actor source of truth is the shared log stream `actor:<actor_id>`. Metadata is a
materialized current view. Replay applies:

```text
ActorCreated -> ActorOwnershipGranted -> ActorCommandSubmitted -> ActorCommandApplied -> ActorSnapshotCreated
```

LogServe Phase 3 provides exactly-once-ish actor command application, not strict
distributed exactly-once execution. A worker may execute a method more than once
after failure or redelivery, but the control plane applies a command to actor
state through an idempotent log key:

```text
actor_id + actor_call_id + applied
```

Each actor command is assigned a monotonic `command_seq` when it is submitted.
The control plane writes `ActorCommandSubmitted` before enqueueing the actor
task, and accepts completion only when:

```text
completion.command_seq == actor.command_count + 1
```

This prevents a later command from being applied ahead of an earlier command in
the actor stream.

The mailbox is enforced in the control plane with one short submission lock per
actor plus dispatch-time gating. Calls for the same actor are assigned monotonic
`command_seq` values and can wait for their own results without blocking later
commands from entering the mailbox. A worker only receives an actor task when
its `command_seq` is the next command after `actor.command_count`, and the
leased task is populated with the latest actor state, owner, and epoch at poll
time.

Actor ownership is represented by `owner_worker_id` and a monotonically
increasing `epoch`. Actor tasks are routed only to the owner. If the owner stops
heartbeating past the lease window, the control plane grants a higher epoch to
another active worker. Completion from an old owner or old epoch is rejected
before writing `ActorCommandApplied`.

Snapshots are written through the result-store interface and the actor log keeps
only `snapshot_ref`. The local development adapter stores snapshot objects under
`local://actors/<actor_id>/snapshots/...`; the Compose stack includes MinIO, and
the same result-store boundary is where an S3-compatible MinIO adapter should be
plugged in for production-style deployments.

When an actor snapshot is created, LogServe records a stream-level logical trim
point in the shared log. `ReadLog` hides records before that point by default,
but segment files are not physically deleted. The retained actor tail starts at
`ActorSnapshotCreated`, which carries the actor metadata needed to replay from
`snapshot_ref` plus later command events. This is snapshot-aware retention, not
full physical compaction.

Observability is emitted as structured logs. Workflow runs include end-to-end
latency and step latency; actor commands include actor id, call id, epoch, and
command count, with replay exposing full versus snapshot command counts.

## Phase 4 LLM Demo

Run the RAG workflow with a mock LLM and three workers:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase4_smoke.ps1
```

The script starts:

```text
worker-1: cached model-A:v1
worker-2: cached model-B:v1
worker-3: empty cache
```

Then it registers `model-A`, enables `LOCALITY_AWARE`, and runs:

```python
@workflow
def rag_with_llm(query: str):
    vec = embed(query)
    docs = search(vec)
    prompt = build_prompt(query, docs)
    return llm_generate("model-A", prompt, version="v1", adapter="mock")
```

Expected output contains:

```text
mock:model-A:v1
```

For a single-node Ubuntu experiment with file-backed mock checkpoints, prepare a
source checkpoint tree and start a worker with a local cache directory:

```bash
mkdir -p /tmp/logserve-checkpoints/model-A-v1
dd if=/dev/zero of=/tmp/logserve-checkpoints/model-A-v1/checkpoint.bin bs=1M count=16

go run ./cmd/logserve-dev \
  --model-source-dir /tmp/logserve-checkpoints \
  --model-cache-dir /tmp/logserve-model-cache \
  --model-cache-capacity-bytes 1073741824
```

In another shell, run the RAG example through the Python SDK:

```bash
export PYTHONPATH=$PWD/sdk/python
export LOGSERVE_SDK_TRANSPORT=grpc
export LOGSERVE_CONTROL_ADDR=127.0.0.1:50052
python examples/rag_llm/workflow.py
```

The mock checkpoint source supports these layouts:

```text
<source>/<model>-<version>/checkpoint.bin
<source>/<model>/<version>/checkpoint.bin
<source>/<model>-<version>.bin
```

## Phase 4 Semantics

The model registry records model name, version, size, path, and adapter. Workers
report local model cache entries during registration and heartbeat. The mock
checkpoint cache copies a checkpoint from `--model-source-dir` into
`--model-cache-dir` on a cold miss, writes a small manifest beside the cached
checkpoint, scans that manifest on worker restart, and evicts least-recently
used checkpoints when `--model-cache-capacity-bytes` is exceeded. A mock LLM
adapter simulates model load and first-token latency on machines without a GPU.
The `vllm` adapter calls a vLLM OpenAI-compatible
`/v1/chat/completions` endpoint using `LOGSERVE_VLLM_BASE_URL` or
`--vllm-base-url`.

Three scheduler policies are implemented:

- `RESOURCE_ONLY`: assign queued LLM work to an idle polling worker without
  considering model cache.
- `LOCALITY_AWARE`: score active workers by cache hit, available capacity, and
  queue wait. Cached workers are preferred while they have capacity; cold workers
  can run work when cached capacity is unavailable.
- `PREDICTED_LATENCY`: use materialized LLM stats and prefer the worker with the
  lowest predicted latency for the requested model/version. Scheduling is an
  `O(number_of_workers)` lookup instead of a replay-all scan over `llm:*`
  streams.

The predicted-latency stats are keyed by `(model_name, model_version,
worker_id)` and maintained from `LLMCompleted` events when LLM tasks finish.
Each entry tracks request count, cache-hit count, EWMA total latency, EWMA model
load latency, EWMA checkpoint fetch latency, and last update time. On control
restart, the stats are rebuilt once from LLM event streams. The runtime
prediction is:

```text
predicted_latency =
  ewma_total_latency_ms
  + queue_penalty
  + cold_start_penalty
  + eviction_penalty
```

LLM requests are task instances with extra model metadata. The worker writes the
LLM event stream `llm:<task_id>`:

```text
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

`ReplayLLM` reconstructs model name/version, worker id, cache hit, checkpoint
fetch time, cache bytes used/capacity, eviction count, model load time,
first-token latency, total latency, and the raw event sequence from that stream.

Covered Phase 4 checks:

- resource-only assigns a model-A request to the first idle worker
- locality-aware waits for the worker that already caches model-A
- predicted-latency can choose a historically faster worker even when another
  worker has the cache
- locality-aware experiment has higher cache hit rate and lower cold-start,
  p95, and p99 latency than resource-only
- mock LLM event replay includes model load and completion metrics
- file-backed checkpoint cache fetches on the first request, hits on the second
  request, and reports persisted cache entries after worker restart
- a RAG workflow can use `llm_generate()` as a real workflow step

## Phase 5 Analysis And Hardening

Phase 5 adds operational analysis assets and runtime hardening:

- running and poll-before-start task redelivery after worker loss
- queue high-watermark and log-append-latency backpressure
- dashboard snapshot API and static dashboard
- benchmark harness for workflow latency, task throughput, actor replay, and
  LLM cold start
- ablation report for locality, snapshots, trimmed replay, and replay semantics
- fault-injection script for worker/control/logd probes, including control
  metadata bootstrap from the shared log
- optional Kubernetes manifests for kind or minikube

Useful commands:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_benchmark.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_fault_injection.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_dashboard.ps1
```

On the Ubuntu single-node experiment machine, use the Linux experiment runner:

```bash
bash scripts/run_experiment.sh
```

By default the runner chooses fresh local ports for logd/control on each run.
Set `LOGSERVE_EXPERIMENT_LOG_ADDR` and `LOGSERVE_EXPERIMENT_CONTROL_ADDR` only
when you intentionally want fixed ports.

The runner creates `reports/experiment-<utc timestamp>/` and writes:

- `environment.txt`: kernel, Go/Python versions, git state
- `command_status.jsonl`: exit code and duration for each verification step
- `logstore_v1_latest.json`: shared-log fsync policy benchmark
- `phase5_benchmark.json`: workflow latency, task throughput, actor snapshot
  ablation, LLM cold/warm cache, locality scheduler comparison
- `checkpoint_cache_probe.json`: file-backed mock checkpoint cache cold/warm
  fetch metrics
- `checkpoint_cache_artifact.log`: file-level proof that the requested
  checkpoint landed in a worker-local model cache directory
- `fault_injection.json`: fault/recovery probe status
- `dashboard_snapshot.json`: final materialized dashboard state
- `summary.md` and `summary.json`: compact report for later writeup

Workload sizes can be increased on the experiment machine without editing code:

```bash
LOGSERVE_BENCH_WORKFLOWS=10 \
LOGSERVE_BENCH_TASKS=100 \
LOGSERVE_BENCH_ACTOR_COMMANDS=100 \
LOGSERVE_BENCH_LLM_REQUESTS=20 \
bash scripts/run_experiment.sh
```

Worker-local executor pool sizing can also be varied without changing scripts:

```bash
LOGSERVE_WORKER_CAPACITY=4 \
LOGSERVE_TASK_POOL_SIZE=4 \
LOGSERVE_LLM_POOL_SIZE=4 \
LOGSERVE_ACTOR_POOL_SIZE=2 \
bash scripts/run_experiment.sh
```

For a quick smoke run, disable the heavier parts:

```bash
LOGSERVE_RUN_RACE=0 LOGSERVE_RUN_LOGSTORE_BENCH=0 bash scripts/run_experiment.sh
```

### Latest Single-Node Experiment Snapshot

The latest validated Ubuntu single-node run used 3 workers, mock LLM serving,
and file-backed checkpoint cache:

```text
reports/experiment-20260610T013044794327660Z
Linux lab2439 6.8.0-111-generic x86_64 GNU/Linux
```

All verification steps passed: Go tests, `go vet`, race tests for control and
worker packages, Python unittest/compile checks, gRPC dependency check,
logstore benchmark, fault-injection tests, Phase 5 benchmark, checkpoint cache
probe, checkpoint artifact check, and dashboard snapshot.

Representative results:

| Metric | Result |
|---|---:|
| Workflow p95 / p99 latency | 823 ms / 823 ms |
| Task throughput | 5.17 tasks/s |
| Task p99 latency | 207 ms |
| Actor snapshot replay commands | 1 vs 21 full replay |
| Actor trimmed replay commands | 1 |
| Resource-only cache hit rate | 0.833 |
| Locality-aware cache hit rate | 1.000 |
| Resource-only p95 latency | 305 ms |
| Locality-aware p95 latency | 205 ms |
| Checkpoint cold fetch | 1 ms |
| Checkpoint warm fetch | 0 ms |
| Checkpoint cache used / capacity | 3,145,728 / 16,777,216 bytes |

The checkpoint probe also verified the worker-local artifact:

```text
runtime/model-cache/worker-1/model-D-v1.checkpoint
```

See `docs/report.md` for the full written experiment summary and
`docs/resume.md` for resume-ready project wording.
