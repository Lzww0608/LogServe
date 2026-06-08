# LogServe

LogServe is a lightweight shared-log-based runtime for AI workflow infrastructure.
Phase 1 focuses on the smallest distributed task execution path:

1. Python SDK submits a `@task`.
2. Control plane appends `TaskSubmitted` and queues the task.
3. Worker heartbeats, polls, appends `TaskStarted`, executes Python, and appends `TaskCompleted`.
4. Task status can be queried from the control plane.
5. Shared log can be read by stream for replay/debugging.

Phase 2 adds a multi-step workflow runtime with replay and retry. Phase 3 adds
stateful Python actors with mailbox serialization, log replay, snapshots, and
epoch fencing. Phase 4 adds LLM serving with model registry, worker model-cache
reporting, and locality-aware scheduling.

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
deployments/         Docker Compose skeleton for Phase 1 infra
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
ActorCreated -> ActorOwnershipGranted -> ActorCommandApplied -> ActorSnapshotCreated
```

LogServe Phase 3 provides exactly-once-ish actor command application, not strict
distributed exactly-once execution. A worker may execute a method more than once
after failure or redelivery, but the control plane applies a command to actor
state through an idempotent log key:

```text
actor_id + actor_call_id + applied
```

The mailbox is enforced in the control plane with one lock per actor. While this
single control-plane implementation is running, only one call for a given actor
can be scheduled and committed at a time, so in-memory actor state is not written
concurrently.

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

## Phase 4 Semantics

The model registry records model name, version, size, path, and adapter. Workers
report local model cache entries during registration and heartbeat. A mock LLM
adapter simulates cold model load and first-token latency on machines without a
GPU. The `vllm` adapter calls a vLLM OpenAI-compatible
`/v1/chat/completions` endpoint using `LOGSERVE_VLLM_BASE_URL` or
`--vllm-base-url`.

Three scheduler policies are implemented:

- `RESOURCE_ONLY`: assign queued LLM work to an idle polling worker without
  considering model cache.
- `LOCALITY_AWARE`: score active workers by cache hit, available capacity, and
  queue wait. Cached workers are preferred while they have capacity; cold workers
  can run work when cached capacity is unavailable.
- `PREDICTED_LATENCY`: replay recent `llm:*` completion events and prefer the
  worker with the lowest observed latency for the requested model/version,
  adjusted by current worker load.

LLM requests are task instances with extra model metadata. The worker writes the
LLM event stream `llm:<task_id>`:

```text
ModelLoadStarted -> ModelLoaded -> LLMCompleted
```

`ReplayLLM` reconstructs model name/version, worker id, cache hit, model load
time, first-token latency, total latency, and the raw event sequence from that
stream.

Covered Phase 4 checks:

- resource-only assigns a model-A request to the first idle worker
- locality-aware waits for the worker that already caches model-A
- predicted-latency can choose a historically faster worker even when another
  worker has the cache
- locality-aware experiment has higher cache hit rate and lower cold-start,
  p95, and p99 latency than resource-only
- mock LLM event replay includes model load and completion metrics
- a RAG workflow can use `llm_generate()` as a real workflow step

## Phase 5 Analysis And Hardening

Phase 5 adds operational analysis assets and runtime hardening:

- running and poll-before-start task redelivery after worker loss
- queue high-watermark and log-append-latency backpressure
- dashboard snapshot API and static dashboard
- benchmark harness for workflow latency, task throughput, actor replay, and
  LLM cold start
- ablation report for locality, snapshots, and replay semantics
- fault-injection script for worker/control/logd probes, including control
  metadata bootstrap from the shared log
- optional Kubernetes manifests for kind or minikube

Useful commands:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_benchmark.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_fault_injection.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\phase5_dashboard.ps1
```
