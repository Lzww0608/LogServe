# LogServe Experiment Summary

- Verdict: **PASS**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/compose_experiment`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/compose_experiment/experiment-package.tar.gz`
- Mode: `compose`

## Checks

| Check | Status | Detail |
|---|---:|---|
| `all_recorded_commands_pass` | PASS | all recorded commands passed |
| `logstore_relaxed_fsync_faster_than_always` | PASS | batch and interval append throughput are greater than always |
| `locality_cache_hit_not_worse_than_resource_only` | PASS | locality-aware cache hit rate should not be lower than resource-only |
| `checkpoint_warm_cache_hit` | PASS | warm checkpoint request hit cache |
| `actor_snapshot_replay_less_than_full` | PASS | snapshot replay should use fewer commands than full replay |
| `dashboard_has_three_workers` | PASS | dashboard contains at least three workers |
| `dashboard_replay_consistent` | PASS | dashboard workflow/actor entries match replay state |

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 1 | `build_logservectl.log` |
| `scheduler_benchmark` | PASS | 6 | `scheduler_benchmark.log` |
| `metadata_benchmark` | PASS | 11 | `metadata_benchmark.log` |
| `logstore_benchmark` | PASS | 12 | `logstore_benchmark.log` |
| `fault_injection_go_tests` | PASS | 6 | `fault_injection_go_tests.log` |
| `compose_build` | PASS | 17 | `compose_build.log` |
| `runtime_compose_start` | PASS | 0 | `compose_up.log` |
| `runtime_logd_ready` | PASS | 0 | `compose.log` |
| `runtime_control_ready` | PASS | 0 | `compose.log` |
| `runtime_workers_ready` | PASS | 0 | `runtime_workers_ready.log` |
| `postgres_before_benchmark` | PASS | 0 | `postgres_before_benchmark.stderr.log` |
| `benchmark` | PASS | 16 | `benchmark.stderr.log` |
| `postgres_after_benchmark` | PASS | 1 | `postgres_after_benchmark.stderr.log` |
| `postgres_benchmark_stats` | PASS | 0 | `postgres_benchmark_stats.stderr.log` |
| `checkpoint_cache_probe` | PASS | 0 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `dashboard_replay_consistency` | PASS | 0 | `dashboard_replay_consistency.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Benchmark

- `workflow_p95_ms`: 920
- `workflow_p99_ms`: 920
- `task_throughput_tps`: 5.110
- `task_p99_latency_ms`: 208
- `actor_snapshot_replay_commands`: 1
- `actor_trimmed_replay_commands`: 1
- `actor_full_replay_commands`: 21
- `actor_no_snapshot_replay_commands`: 21
- `actor_compactable_log_records`: 45
- `actor_compactable_log_bytes`: 18382
- `llm_cold_total_latency_ms`: 20
- `llm_warm_total_latency_ms`: 16
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 1
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 1.000
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 209
- `locality_aware_p95_latency_ms`: 208
- `predicted_latency_p95_latency_ms`: 209

## Logstore Benchmark

- `logstore_always_append_tps`: 1763.388
- `logstore_always_read_tps`: 516392.732
- `logstore_always_recover_ms`: 40
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 355954.908
- `logstore_batch_read_tps`: 641520.599
- `logstore_batch_recover_ms`: 44
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 305165.579
- `logstore_interval_read_tps`: 530188.833
- `logstore_interval_recover_ms`: 44
- `logstore_interval_segments`: 7

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_worker_id`: worker-a
- `checkpoint_warm_worker_id`: worker-a
- `checkpoint_cold_fetch_ms`: 1
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 3145728
- `checkpoint_cache_capacity_bytes`: 16777216
- `checkpoint_validation_errors`: []

## Fault Injection

- `worker_kill_recovery`: passed
- `queue_redelivery`: passed
- `control_restart_probe`: passed
- `logd_restart_probe`: covered_by_logstore_recovery_and_process_logs
- `source`: go test ./tests/integration fault and recovery subset

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 84
- `dashboard_workflows`: 3
- `dashboard_actors`: 2
- `dashboard_workers`: 3
- `dashboard_models`: 3
- `dashboard_compactable_log_records`: 45
- `dashboard_compactable_log_bytes`: 18382
- `metadata_materializer_mode`: sync
- `metadata_materializer_pending_deltas`: n/a
- `metadata_materializer_flush_count`: n/a
- `metadata_materializer_flush_errors`: n/a
- `metadata_materializer_lag_ms`: n/a

## PostgreSQL Benchmark Stats

- `postgres_mode`: sync
- `postgres_elapsed_ms`: 16811
- `postgres_transactions_delta`: 1287
- `postgres_row_writes_delta`: 1801
- `postgres_transactions_per_sec`: 76.557
- `postgres_row_writes_per_sec`: 107.132

## Dashboard Replay Consistency

- `dashboard_replay_consistent`: True
- `dashboard_replay_workflows`: 3
- `dashboard_replay_actors`: 2
- `dashboard_replay_checked`: 5
- `dashboard_replay_failures`: []

## Go Benchmarks

### scheduler_benchmark.log
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=1-32`: ns_per_op=592.000, bytes_per_op=105.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=10-32`: ns_per_op=2010.000, bytes_per_op=367.000, allocs_per_op=4.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=100-32`: ns_per_op=19591.000, bytes_per_op=2480.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=1-32`: ns_per_op=419.400, bytes_per_op=128.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=10-32`: ns_per_op=1602.000, bytes_per_op=344.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=100-32`: ns_per_op=14962.000, bytes_per_op=2494.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=1-32`: ns_per_op=477.100, bytes_per_op=151.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=10-32`: ns_per_op=1468.000, bytes_per_op=346.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=100-32`: ns_per_op=3834.000, bytes_per_op=2460.000, allocs_per_op=5.000
### metadata_benchmark.log
- `BenchmarkMemoryStoreConcurrentGetTask/legacy-32`: ns_per_op=59.890, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentGetTask/v2-32`: ns_per_op=27.860, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/legacy-32`: ns_per_op=1719.000, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/v2-32`: ns_per_op=4347.000, bytes_per_op=294.000, allocs_per_op=3.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/legacy-32`: ns_per_op=1440.000, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/v2-32`: ns_per_op=170.900, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/legacy-32`: ns_per_op=10514.000, heartbeat_p99_batch_ns=5799754.000, bytes_per_op=3361.000, allocs_per_op=30.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/v2-32`: ns_per_op=5718.000, heartbeat_p99_batch_ns=24462.000, bytes_per_op=3834.000, allocs_per_op=36.000
- `BenchmarkMemoryStoreActiveWorkers/legacy-32`: ns_per_op=58717.000, bytes_per_op=369540.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreActiveWorkers/v2-32`: ns_per_op=73071.000, bytes_per_op=369539.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreUpdateWorkflow/legacy-32`: ns_per_op=14552.000, bytes_per_op=14067.000, allocs_per_op=45.000
- `BenchmarkMemoryStoreUpdateWorkflow/v2-32`: ns_per_op=20472.000, bytes_per_op=18592.000, allocs_per_op=40.000

## Raw Files

- `benchmark.json`
- `benchmark.stderr.log`
- `bin`
- `build_logservectl.log`
- `checkpoint_cache_artifact.log`
- `checkpoint_cache_probe.json`
- `checkpoint_cache_probe.stderr.log`
- `command_status.jsonl`
- `compose.env`
- `compose_build.log`
- `compose_up.log`
- `dashboard_replay_consistency.json`
- `dashboard_replay_consistency.stderr.log`
- `dashboard_snapshot.json`
- `dashboard_snapshot.stderr.log`
- `dashboard_wait.json`
- `dashboard_wait.stderr.log`
- `environment.txt`
- `experiment-package.tar.gz`
- `fault_injection.json`
- `fault_injection_go_tests.log`
- `logstore_benchmark.log`
- `logstore_latest.json`
- `metadata_benchmark.log`
- `metadata_heap.pprof`
- `metadata_mutex.pprof`
- `package.log`
- `postgres_after_benchmark.json`
- `postgres_after_benchmark.stderr.log`
- `postgres_before_benchmark.json`
- `postgres_before_benchmark.stderr.log`
- `postgres_benchmark_stats.json`
- `postgres_benchmark_stats.stderr.log`
- `python_compileall.log`
- `python_grpc_deps.log`
- `python_pip_install.log`
- `python_unittest.log`
- `python_venv_create.log`
- `runtime_workers_ready.log`
- `scheduler_benchmark.log`
- `summarize_experiment.log`
- `summary.json`
- `summary.md`
