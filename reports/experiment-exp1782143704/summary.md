# LogServe Experiment Summary

- Verdict: **FAIL**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782143704`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782143704/experiment-package.tar.gz`
- Mode: `compose`

## Checks

| Check | Status | Detail |
|---|---:|---|
| `all_recorded_commands_pass` | FAIL | one or more recorded commands failed |
| `logstore_relaxed_fsync_faster_than_always` | WARN | logstore benchmark policy data is missing |
| `locality_cache_hit_not_worse_than_resource_only` | WARN | locality benchmark did not run |
| `checkpoint_warm_cache_hit` | PASS | warm checkpoint request hit cache |
| `actor_snapshot_replay_less_than_full` | WARN | actor replay ablation data is missing |
| `dashboard_has_three_workers` | PASS | dashboard contains at least three workers |

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 2 | `build_logservectl.log` |
| `compose_build` | PASS | 11 | `compose_build.log` |
| `runtime_compose_start` | PASS | 73 | `compose_up.log` |
| `runtime_logd_ready` | PASS | 0 | `compose.log` |
| `runtime_control_ready` | PASS | 0 | `compose.log` |
| `runtime_workers_ready` | PASS | 0 | `runtime_workers_ready.log` |
| `benchmark` | FAIL | 1 | `benchmark.stderr.log` |
| `checkpoint_cache_probe` | PASS | 1 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_worker_id`: worker-a
- `checkpoint_warm_worker_id`: worker-a
- `checkpoint_cold_fetch_ms`: 1
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 1048576
- `checkpoint_cache_capacity_bytes`: 16777216
- `checkpoint_validation_errors`: []

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 5
- `dashboard_workflows`: 1
- `dashboard_actors`: 0
- `dashboard_workers`: 3
- `dashboard_models`: 3
- `dashboard_compactable_log_records`: n/a
- `dashboard_compactable_log_bytes`: n/a

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
