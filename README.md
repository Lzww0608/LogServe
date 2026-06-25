# LogServe

LogServe is a shared-log-based runtime for AI workflows. It includes a
log-first control plane, Python SDK, workers, workflow DAG runtime, stateful
actors, and LLM serving with model-cache-aware scheduling.

Main pieces:

1. Distributed task execution through `@task`, shared-log task events, and
   worker polling.
2. Multi-step workflow execution with DAG scheduling, replay, retry, timeout,
   idempotency, and result references.
3. Stateful Python actors with mailbox serialization, command sequencing,
   snapshot replay, ownership transfer, and epoch fencing.
4. LLM serving with model registry, mock/vLLM adapters, worker model-cache
   reporting, file-backed checkpoint cache, and locality-aware scheduling.
5. Fault injection, benchmark scripts, ablation studies, dashboard snapshots,
   backpressure, and Kubernetes manifests.

## Repository Layout

```text
cmd/
  logserve-logd      Shared log service
  logserve-control   Control plane and in-memory queue
  logserve-worker    Worker agent and Python executor bridge
  logserve-dev       Single-process local dev runner
  logservectl        CLI fallback for the Python SDK
proto/               gRPC contracts
internal/logstore    Segmented append-only log
internal/control     Task API and status materialization
internal/workflow    Workflow DAG model, argument resolution, replay
internal/actor       Actor state model and replay reducer
internal/worker      Worker polling and task execution
internal/objectstore Local result store for large workflow results
sdk/python/logserve  Python SDK
executor/python      Python function executor
deployments/         Docker Compose and Kubernetes manifests
docs/report.md       Project report and experiment writeup
docs/resume.md       Resume-ready project summary
```

## Local Demo Without Docker

Docker is optional for the local task loop. From the repository root:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_task.ps1
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

Output:

```text
3
```

## Workflow Demo

Run the Python `@workflow` DSL example:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_workflow.ps1
```

Output:

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

## Worker Poll Batching And Long-Poll

Workers no longer rely on a fixed poll tick for every idle cycle. Heartbeat and
task pull are split: heartbeat uses `--heartbeat-ms` (default 1000ms) while poll
uses `--poll-ms` as the long-poll timeout when the local pool is idle.

Control plane `PollTask` accepts:

```text
max_tasks         batch size (up to 64); worker requests idle local capacity
wait_timeout_ms   long-poll wait when no tasks are ready
```

The worker batches `PollTask(max_tasks=idle_capacity)` and dispatches all
returned specs into the local executor pool. Completions are flushed through
`CompleteTasks` in one RPC per result burst.

`notifyTaskAvailable()` wakes long-polling workers as soon as tasks are enqueued,
so low-load scheduling latency does not wait for the next poll interval.

Server-streaming `TaskStream` is intentionally not exposed yet. Unary batch +
long-poll already removes the main empty-spin and tick-delay path; adding push
streaming would require stream flow control, reconnect, lease recovery, and
per-worker backpressure before it becomes a supported protocol surface.

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

## Shared Log Benchmark

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
bash scripts/logstore_benchmark.sh
```

The script writes a generated JSON report to
`benchmarks/logstore_latest.json` and compares append, read, recovery time,
and segment count across `always`, `batch`, and `interval` fsync policies. You
can tune the workload through environment variables such as
`LOGSERVE_LOGBENCH_RECORDS`, `LOGSERVE_LOGBENCH_PAYLOAD_BYTES`, and
`LOGSERVE_LOGBENCH_SEGMENT_SIZE_BYTES`.

Log records use format version 2 with a 40-byte header. Version 1 segments
remain readable: they use a 36-byte header and implicit CRC32 IEEE checksum.
New records default to CRC32C (Castagnoli) via hardware-accelerated
`github.com/klauspost/crc32`; the header extension records the checksum type
(`IEEE`, `CRC32C`, `XXH3`, `None`). Bodies larger than 64 KiB are checksummed
and verified in 64 KiB chunks so large payloads do not require a single
monolithic hash pass. Configure the writer checksum through
`logstore.Options.ChecksumType` (default `CRC32C`). Run checksum benchmarks with:

```bash
go test -mod=mod ./internal/logstore/... -bench=BenchmarkChecksum -benchmem -run='^$'
```

## Serialization And Buffer Pools

Hot paths use pooled buffers and binary codecs where profiling showed
allocation pressure, without replacing JSON globally:

- Log record encode uses `sync.Pool` (`encodeRecordPooled`) for header+body buffers.
- Local object store streaming copies use a pooled 32 KiB buffer.
- Python executor IPC defaults to length-prefixed msgpack frames (`--loop-msgpack`);
  set `LOGSERVE_EXECUTOR_PROTOCOL=json` to force JSON lines. Executor frame reads
  reuse a pooled buffer up to 4 MiB.
- Internal log event payloads for `TaskSubmitted`, workflow events, actor events,
  and task lifecycle (`TaskStarted`/`TaskRedelivered`/`TaskCompleted`/`TaskFailed`)
  use msgpack with `LSE\x01` magic and JSON fallback for older segments.
- `ReadRawEach` / `ReadLogRawEach` let replay reducers consume scratch-backed
  payloads without constructing full `Record` objects. `ReadLogStream` on logd
  streams from the raw path. Control bootstrap, checkpoint replay, task/workflow/
  actor/LLM replay, function registry, and system streams use the raw iterator.
- Workflow `StepScheduled` events persist `resolved_args_json` so retries and
  log-only bootstrap can hit `ResolveCachedArgs` without re-marshaling step args.

CLI and dashboard surfaces remain JSON. Migrate additional event types only when
profiling shows marshal/unmarshal in the flame graph.

## Mmap Read Experiment (Linux/macOS)

Sealed log segments can be read through a read-only `mmap` mapping instead of
per-record `ReadAt` syscalls. The active segment always uses `ReadAt`. Enable
with:

```bash
export LOGSERVE_LOG_MMAP_READ=1
```

`Store.MmapReadStats()` reports mapped segment count and bytes. Compaction unmaps
cached readers before deleting segment files. Windows builds disable mmap read.

Compare read/replay benchmarks:

```bash
go test ./internal/logstore/... -bench=BenchmarkReadRawLargeStream -benchmem -run='^$'
```

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
materialized dashboard/task/workflow/actor/model view to PostgreSQL. PostgreSQL
writes default to `LOGSERVE_POSTGRES_MODE=sync`; set
`LOGSERVE_POSTGRES_MODE=async` to enqueue metadata deltas and flush them in
coalesced background batches. To compare sync and async Compose runs, use
`bash scripts/postgres_async_compare.sh`; it records task throughput, p99, and
PostgreSQL transaction/write rates under `reports/postgres-async-compare-*`.
The comparison treats task throughput/p99 as non-regression checks by default
(`LOGSERVE_COMPARE_TASK_THROUGHPUT_MIN_RATIO=0.99`,
`LOGSERVE_COMPARE_TASK_P99_MAX_RATIO=1.0`) while reporting exact ratios and
strict-improvement observations.
If the PostgreSQL tables are dropped, restart control after logd and `BootstrapFromLog`
will recreate the tables and rebuild the view from shared log streams.
Metadata checkpoints can shorten replay by writing reducer state to
`system:checkpoints`; enable the background writer with
`--metadata-checkpoint-interval-ms` or `LOGSERVE_METADATA_CHECKPOINT_INTERVAL_MS`.
Each checkpoint records per-stream `last_seq` plus task specs/terminal state,
workflow state, actor state, and materialized LLM stats. On restart, control
loads the latest valid checkpoint and reads each covered stream tail from
`last_seq+1`; if no valid checkpoint is available, it falls back to full replay.
`--metadata-checkpoint-retention` / `LOGSERVE_METADATA_CHECKPOINT_RETENTION`
controls how many checkpoint records are retained, defaulting to 3.

## Tests

```powershell
go test ./...
```

On Ubuntu/Linux with the `mcts` conda environment (Python executor msgpack deps):

```bash
bash scripts/test_conda_mcts.sh
```

Override the conda env with `LOGSERVE_CONDA_ENV` if needed.

Covered task and log checks:

- append/read and idempotent append in shared log
- recovery truncation for partial log tail
- worker heartbeat, task execution, status query, and task event log chain

Covered workflow checks:

- `simple_rag` workflow completion
- worker stops after `embed`; a restarted worker continues from `search` without re-running `embed`
- replayed workflow state from shared log matches metadata state
- duplicate step completion does not write a second workflow final result
- failed steps retry according to `max_attempts`
- timed-out steps fail and retry according to `max_attempts`
- poll-before-start worker loss redelivers the leased task
- ordinary ad-hoc task specs are restored from `TaskSubmitted` after control restart
- stale task completions are rejected by task lease epoch

## Workflow Semantics

Workflow source of truth is the shared log. Metadata is a materialized current
view and can be checked against replay through `ReplayWorkflow`.

LogServe provides exactly-once-ish workflow step results, not strict
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

### Runtime DAG Scheduling

Workflow runtime state keeps external JSON `step_id` keys, but internally uses a
topologically ordered `[]StepState` plus a `RuntimeDAG` view:

- `byID` maps `step_id` to slice index (built once after `ParseDefinition`).
- `remainingDeps` tracks unresolved upstream count per step.
- `outgoing` lists downstream indices for incremental updates.
- `ready` is a queue of step indices eligible for scheduling.

`scheduleReadySteps` pops from the ready queue via `PopReadyStep` instead of
scanning every definition step and map lookup on each schedule pass. When a step
succeeds, `SetStepSucceeded` only walks its outgoing edges to decrement
`remainingDeps` and enqueue newly ready steps.

Replay and legacy persisted state still accept the historical
`map[string]StepState` JSON shape; `UnmarshalJSON` normalizes into the slice
layout and calls `RebuildRuntime` to reconstruct the ready frontier. Retry,
timeout, and duplicate-completion semantics are unchanged.

Benchmark the ready-queue path vs a legacy full-scan baseline:

```bash
go test ./internal/workflow -bench='Schedule.*DAG' -benchmem
```

## Actor Demo

Run the Python `@actor` counter example:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_actor.ps1
```

Output:

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

Covered actor checks:

- `Counter` actor recovery after the first worker exits at 100 `inc()` calls
- a second worker takes ownership and `get()` returns `100`
- replayed actor state from `actor:<actor_id>` matches metadata state
- snapshots reduce replay work compared with full command replay
- 1000 concurrent `inc()` submissions serialize through the actor mailbox and
  final `get()` returns `1000`
- stale actor completions are rejected by worker id plus epoch fencing

## Actor Semantics

Actor source of truth is the shared log stream `actor:<actor_id>`. Metadata is a
materialized current view. Replay applies:

```text
ActorCreated -> ActorOwnershipGranted -> ActorCommandSubmitted -> ActorCommandApplied -> ActorSnapshotCreated
```

LogServe provides exactly-once-ish actor command application, not strict
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
plugged in for deployments that need S3-compatible storage.

When an actor snapshot is created, LogServe records a stream-level logical trim
point in the shared log. `ReadLog` hides records before that point by default,
but segment files are not physically deleted. The retained actor tail starts at
`ActorSnapshotCreated`, which carries the actor metadata needed to replay from
`snapshot_ref` plus later command events. This is snapshot-aware retention, not
full physical compaction.

Observability is emitted as structured logs. Workflow runs include end-to-end
latency and step latency; actor commands include actor id, call id, epoch,
command count, and payload byte sizes (`state_json_bytes`, `args_json_bytes`,
`result_json_bytes`), with replay exposing full versus snapshot command counts.

### Actor Mailbox And Scheduler v2

Actor commands are not mixed with the general FIFO queue when scheduler v2 is
enabled (`LOGSERVE_SCHEDULER_V2=1`). The indexed scheduler routes actor tasks into
`actorPending[actor_id]` deques. Owner workers poll only their owned actor IDs,
and `command_seq == command_count + 1` gating prevents a blocked future command
from head-of-line blocking unrelated general or LLM tasks.

Per-actor mutex tables on control and worker use refcount-free idle eviction:
after unlock, an entry is removed when no other goroutine holds the lock. Empty
`actorPending` deques are pruned after the last task is assigned so actor count
does not leak map entries over long runtimes.

Large actor state shipped on each poll is logged as `actor_command_dispatched`
when `state_json_bytes` exceeds 4 KiB. Periodic snapshots already trim the actor
log tail; delta snapshots for high-frequency large-state actors remain a future
optimization path.

## LLM Demo

Run the RAG workflow with a mock LLM and three workers:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\smoke_llm.ps1
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

Output contains:

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

## LLM Semantics

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

### LLM Placement Index

Scheduler state incrementally maintains per-model placement heaps instead of
scanning all active workers on every poll:

- Heartbeat / task lease updates call `UpsertWorker` to refresh capacity,
  cached models, and heap membership.
- `LLMCompleted` updates EWMA stats and re-scores the worker in the model's
  predicted heap.
- `LOCALITY_AWARE` reads cached/cold heaps (cache hit, capacity, queue wait).
- `PREDICTED_LATENCY` walks the model's predicted heap ordered by EWMA latency,
  cold-start penalty, eviction risk, and running-task queue penalty.

`PollTask` no longer calls a full `ListWorkers` sync on every indexed poll;
workers are hydrated from heartbeat updates, with a one-time active-worker sync
only when the placement index is empty after restart.

Benchmark placement-index selection vs full worker scan:

```bash
go test ./internal/control -bench='Preferred(Locality|Predicted)' -benchmem -run='^$'
```

The predicted-latency stats are keyed by `(model_name, model_version,
worker_id)` and maintained from `LLMCompleted` events when LLM tasks finish.
Each entry tracks request count, cache-hit count, EWMA total latency, EWMA model
load latency, EWMA checkpoint fetch latency, and last update time. On control
restart, the stats are restored from the latest metadata checkpoint plus each
`llm:*` stream tail when a checkpoint is available; otherwise they are rebuilt
once from LLM event streams. The runtime prediction is:

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

Covered LLM checks:

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

## Benchmarks, Profiles, And Regression Gates

Each optimization should ship with:

1. Correctness tests (`go test ./...`).
2. Microbenchmark before/after (`bash scripts/benchmark_micro.sh`).
3. Macro benchmark (`bash scripts/run_experiment.sh` or `examples/evaluation/benchmark.py`).
4. pprof evidence (`bash scripts/collect_pprof.sh`).
5. A documented rollback flag (for example `LOGSERVE_SCHEDULER_V2`, `LOGSERVE_LOG_MMAP_READ`).

### Microbenchmarks

| Area | Command |
|------|---------|
| Logstore append/recover | `go test ./internal/logstore -bench 'BenchmarkStoreAppend|BenchmarkStoreRecover' -benchmem` |
| Logstore read | `go test ./internal/logstore -bench 'BenchmarkRead' -benchmem` |
| Control scheduler | `go test ./internal/control -bench 'BenchmarkSchedulerAssignMixedBacklog|BenchmarkPreferred' -benchmem` |
| Metadata | `go test ./internal/metadata -bench BenchmarkMemoryStore -benchmem` |
| Bootstrap | `go test ./internal/control -bench BenchmarkBootstrapFromLog -benchmem` |
| Workflow DAG | `go test ./internal/workflow -bench 'BenchmarkSchedule' -benchmem` |

Run the full micro suite and emit JSON:

```bash
bash scripts/benchmark_micro.sh
python3 scripts/compare_benchmark.py benchmarks/baseline.json benchmarks/micro-<timestamp>.json
```

### Runtime pprof

Services accept `--pprof-addr` or `LOGSERVE_PPROF_ADDR`. Mutex/block profiling is enabled via
`LOGSERVE_MUTEX_PROFILE_FRACTION` and `LOGSERVE_BLOCK_PROFILE_RATE`.

```bash
LOGSERVE_PPROF_ADDR=127.0.0.1:6062 ./cmd/logserve-control/control ...
bash scripts/collect_pprof.sh 127.0.0.1:6062 benchmarks/profiles
```

### Macro experiment pipeline

`scripts/run_experiment.sh` runs correctness, race, Go microbenches, logstore macro bench,
runtime macro benchmark, checkpoint cache probe/bench, executor bench, optional pprof capture,
and `scripts/summarize_experiment.py`.

## Analysis And Hardening

Operational checks and experiment runners live in `scripts/` and write their
results under `reports/`. README keeps only the entry points; detailed numbers
belong in the docs:

- `docs/report.md`: project report and accepted experiment results.
- `docs/ubuntu-project-acceptance.md`: top-level Ubuntu single-server
  project acceptance procedure and expected handoff files.
- `docs/ubuntu-postgres-async-test.md`: PostgreSQL async materializer procedure
  and accepted comparison.
- `docs/ubuntu-checkpoint-acceptance.md`: metadata checkpoint bootstrap
  procedure and accepted checkpoint result.
- `docs/resume.md`: resume-ready project wording.

Common local checks:

```powershell
powershell.exe -ExecutionPolicy Bypass -File .\scripts\benchmark.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\fault_injection.ps1
powershell.exe -ExecutionPolicy Bypass -File .\scripts\dashboard_snapshot.ps1
```

Ubuntu single-node runners:

```bash
bash scripts/ubuntu_project_acceptance.sh
bash scripts/run_experiment.sh
bash scripts/ubuntu_postgres_async_acceptance.sh
bash scripts/ubuntu_checkpoint_acceptance.sh
```

Use `scripts/ubuntu_project_acceptance.sh` as the top-level single-server
project acceptance entry point. Use `scripts/run_experiment.sh` for the broader
compose benchmark/fault-injection suite, and use the two focused
`ubuntu_*_acceptance.sh` wrappers when validating the PostgreSQL async
materializer or metadata checkpoint bootstrap changes. The wrappers generate
`acceptance_summary.md`, `acceptance_summary.json`, and a packaged tarball in
`reports/`.
