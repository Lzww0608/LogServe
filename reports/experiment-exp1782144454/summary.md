# LogServe Experiment Summary

- Verdict: **PASS**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782144454`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782144454/experiment-package.tar.gz`
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

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `go_test_all` | PASS | 27 | `go_test_all.log` |
| `go_vet` | PASS | 1 | `go_vet.log` |
| `go_race_control_metadata_worker` | PASS | 7 | `go_race_control_metadata_worker.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 1 | `build_logservectl.log` |
| `scheduler_benchmark` | PASS | 7 | `scheduler_benchmark.log` |
| `metadata_benchmark` | PASS | 9 | `metadata_benchmark.log` |
| `logstore_benchmark` | PASS | 12 | `logstore_benchmark.log` |
| `fault_injection_go_tests` | PASS | 6 | `fault_injection_go_tests.log` |
| `compose_build` | PASS | 2 | `compose_build.log` |
| `runtime_compose_start` | PASS | 1 | `compose_up.log` |
| `runtime_logd_ready` | PASS | 0 | `compose.log` |
| `runtime_control_ready` | PASS | 0 | `compose.log` |
| `runtime_workers_ready` | PASS | 0 | `runtime_workers_ready.log` |
| `benchmark` | PASS | 45 | `benchmark.stderr.log` |
| `checkpoint_cache_probe` | PASS | 1 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Benchmark

- `workflow_p95_ms`: 1244
- `workflow_p99_ms`: 1244
- `task_throughput_tps`: 4.870
- `task_p99_latency_ms`: 521
- `actor_snapshot_replay_commands`: 1
- `actor_trimmed_replay_commands`: 1
- `actor_full_replay_commands`: 21
- `actor_no_snapshot_replay_commands`: 21
- `actor_compactable_log_records`: 45
- `actor_compactable_log_bytes`: 18283
- `llm_cold_total_latency_ms`: 98
- `llm_warm_total_latency_ms`: 18
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 1
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 1.000
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 209
- `locality_aware_p95_latency_ms`: 209
- `predicted_latency_p95_latency_ms`: 209

## Logstore Benchmark

- `logstore_always_append_tps`: 1734.660
- `logstore_always_read_tps`: 448881.025
- `logstore_always_recover_ms`: 42
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 285940.289
- `logstore_batch_read_tps`: 656219.478
- `logstore_batch_recover_ms`: 44
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 334596.961
- `logstore_interval_read_tps`: 700969.374
- `logstore_interval_recover_ms`: 41
- `logstore_interval_segments`: 7

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_worker_id`: worker-a
- `checkpoint_warm_worker_id`: worker-a
- `checkpoint_cold_fetch_ms`: 1
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 2097152
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
- `dashboard_tasks`: 218
- `dashboard_workflows`: 3
- `dashboard_actors`: 2
- `dashboard_workers`: 3
- `dashboard_models`: 3
- `dashboard_compactable_log_records`: 45
- `dashboard_compactable_log_bytes`: 18283

## Go Benchmarks

### scheduler_benchmark.log
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=1-32`: ns_per_op=537.100, bytes_per_op=105.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=10-32`: ns_per_op=2305.000, bytes_per_op=367.000, allocs_per_op=4.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=100-32`: ns_per_op=16946.000, bytes_per_op=2480.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=1-32`: ns_per_op=534.200, bytes_per_op=128.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=10-32`: ns_per_op=1759.000, bytes_per_op=344.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=100-32`: ns_per_op=16121.000, bytes_per_op=2500.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=1-32`: ns_per_op=487.400, bytes_per_op=151.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=10-32`: ns_per_op=1501.000, bytes_per_op=346.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=100-32`: ns_per_op=3910.000, bytes_per_op=2464.000, allocs_per_op=5.000
### metadata_benchmark.log
- `BenchmarkMemoryStoreConcurrentGetTask/legacy-32`: ns_per_op=69.440, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentGetTask/v2-32`: ns_per_op=27.540, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/legacy-32`: ns_per_op=1400.000, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/v2-32`: ns_per_op=4243.000, bytes_per_op=296.000, allocs_per_op=3.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/legacy-32`: ns_per_op=1487.000, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/v2-32`: ns_per_op=195.000, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/legacy-32`: ns_per_op=10079.000, heartbeat_p99_batch_ns=5505106.000, bytes_per_op=3360.000, allocs_per_op=30.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/v2-32`: ns_per_op=7040.000, heartbeat_p99_batch_ns=26323.000, bytes_per_op=3820.000, allocs_per_op=36.000
- `BenchmarkMemoryStoreActiveWorkers/legacy-32`: ns_per_op=59624.000, bytes_per_op=369537.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreActiveWorkers/v2-32`: ns_per_op=67272.000, bytes_per_op=369540.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreUpdateWorkflow/legacy-32`: ns_per_op=15336.000, bytes_per_op=14067.000, allocs_per_op=45.000
- `BenchmarkMemoryStoreUpdateWorkflow/v2-32`: ns_per_op=21143.000, bytes_per_op=18590.000, allocs_per_op=40.000

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
- `dashboard_snapshot.json`
- `dashboard_snapshot.stderr.log`
- `dashboard_wait.json`
- `dashboard_wait.stderr.log`
- `environment.txt`
- `experiment-package.tar.gz`
- `fault_injection.json`
- `fault_injection_go_tests.log`
- `go_race_control_metadata_worker.log`
- `go_test_all.log`
- `go_vet.log`
- `logstore_benchmark.log`
- `logstore_latest.json`
- `metadata_benchmark.log`
- `metadata_heap.pprof`
- `metadata_mutex.pprof`
- `package.log`
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
