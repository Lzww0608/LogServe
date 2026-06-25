# LogServe Experiment Summary

- Verdict: **WARN**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T025707Z/postgres_async_acceptance/postgres_async_compare/async`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T025707Z/postgres_async_acceptance/postgres_async_compare/async/experiment-package.tar.gz`
- Mode: `compose`

## Checks

| Check | Status | Detail |
|---|---:|---|
| `all_recorded_commands_pass` | PASS | all recorded commands passed |
| `logstore_relaxed_fsync_faster_than_always` | WARN | logstore benchmark policy data is missing |
| `locality_cache_hit_not_worse_than_resource_only` | PASS | locality-aware cache hit rate should not be lower than resource-only |
| `checkpoint_warm_cache_hit` | PASS | warm checkpoint request hit cache |
| `actor_snapshot_replay_less_than_full` | PASS | snapshot replay should use fewer commands than full replay |
| `dashboard_has_three_workers` | PASS | dashboard contains at least three workers |
| `dashboard_replay_consistent` | PASS | dashboard workflow/actor entries match replay state |

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 4 | `python_pip_install.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 0 | `build_logservectl.log` |
| `compose_build` | PASS | 2 | `compose_build.log` |
| `runtime_compose_start` | PASS | 1 | `compose_up.log` |
| `runtime_logd_ready` | PASS | 0 | `compose.log` |
| `runtime_control_ready` | PASS | 0 | `compose.log` |
| `runtime_workers_ready` | PASS | 0 | `runtime_workers_ready.log` |
| `postgres_before_benchmark` | PASS | 0 | `postgres_before_benchmark.stderr.log` |
| `benchmark` | PASS | 39 | `benchmark.stderr.log` |
| `postgres_after_benchmark` | PASS | 1 | `postgres_after_benchmark.stderr.log` |
| `postgres_benchmark_stats` | PASS | 0 | `postgres_benchmark_stats.stderr.log` |
| `checkpoint_cache_probe` | PASS | 0 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `dashboard_replay_consistency` | PASS | 0 | `dashboard_replay_consistency.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Benchmark

- `workflow_p95_ms`: 817
- `workflow_p99_ms`: 817
- `task_throughput_tps`: 5.000
- `task_p99_latency_ms`: 207
- `actor_snapshot_replay_commands`: 1
- `actor_trimmed_replay_commands`: 1
- `actor_full_replay_commands`: 41
- `actor_no_snapshot_replay_commands`: 41
- `actor_compactable_log_records`: 89
- `actor_compactable_log_bytes`: 36722
- `llm_cold_total_latency_ms`: 94
- `llm_warm_total_latency_ms`: 17
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 1
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 1.000
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 206
- `locality_aware_p95_latency_ms`: 206
- `predicted_latency_p95_latency_ms`: 206

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

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 200
- `dashboard_workflows`: 5
- `dashboard_actors`: 2
- `dashboard_workers`: 3
- `dashboard_models`: 3
- `dashboard_compactable_log_records`: 89
- `dashboard_compactable_log_bytes`: 36722
- `metadata_materializer_mode`: async
- `metadata_materializer_pending_deltas`: 6
- `metadata_materializer_flush_count`: 42
- `metadata_materializer_flush_errors`: n/a
- `metadata_materializer_lag_ms`: 746

## PostgreSQL Benchmark Stats

- `postgres_mode`: async
- `postgres_elapsed_ms`: 39891
- `postgres_transactions_delta`: 52
- `postgres_row_writes_delta`: 661
- `postgres_transactions_per_sec`: 1.304
- `postgres_row_writes_per_sec`: 16.570

## Dashboard Replay Consistency

- `dashboard_replay_consistent`: True
- `dashboard_replay_workflows`: 5
- `dashboard_replay_actors`: 2
- `dashboard_replay_checked`: 7
- `dashboard_replay_failures`: []

## Notes

- verdict is not pass; inspect checks and failed command logs before using the numbers in a report.

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
- `summarize_experiment.log`
- `summary.json`
- `summary.md`
