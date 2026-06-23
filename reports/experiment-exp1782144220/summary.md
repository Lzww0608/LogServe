# LogServe Experiment Summary

- Verdict: **WARN**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782144220`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782144220/experiment-package.tar.gz`
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

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 1 | `build_logservectl.log` |
| `compose_build` | PASS | 34 | `compose_build.log` |
| `runtime_compose_start` | PASS | 1 | `compose_up.log` |
| `runtime_logd_ready` | PASS | 0 | `compose.log` |
| `runtime_control_ready` | PASS | 0 | `compose.log` |
| `runtime_workers_ready` | PASS | 0 | `runtime_workers_ready.log` |
| `benchmark` | PASS | 17 | `benchmark.stderr.log` |
| `checkpoint_cache_probe` | PASS | 0 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Benchmark

- `workflow_p95_ms`: 920
- `workflow_p99_ms`: 920
- `task_throughput_tps`: 5.110
- `task_p99_latency_ms`: 209
- `actor_snapshot_replay_commands`: 1
- `actor_trimmed_replay_commands`: 1
- `actor_full_replay_commands`: 21
- `actor_no_snapshot_replay_commands`: 21
- `actor_compactable_log_records`: 45
- `actor_compactable_log_bytes`: 18283
- `llm_cold_total_latency_ms`: 20
- `llm_warm_total_latency_ms`: 18
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 1
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 1.000
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 209
- `locality_aware_p95_latency_ms`: 207
- `predicted_latency_p95_latency_ms`: 207

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

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 84
- `dashboard_workflows`: 3
- `dashboard_actors`: 2
- `dashboard_workers`: 3
- `dashboard_models`: 3
- `dashboard_compactable_log_records`: 45
- `dashboard_compactable_log_bytes`: 18283

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
- `dashboard_snapshot.json`
- `dashboard_snapshot.stderr.log`
- `dashboard_wait.json`
- `dashboard_wait.stderr.log`
- `environment.txt`
- `experiment-package.tar.gz`
- `package.log`
- `python_compileall.log`
- `python_grpc_deps.log`
- `python_pip_install.log`
- `python_unittest.log`
- `python_venv_create.log`
- `runtime_workers_ready.log`
- `summarize_experiment.log`
- `summary.json`
- `summary.md`
