# LogServe Experiment Summary

- Verdict: **FAIL**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782142989`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/experiment-exp1782142989/experiment-package.tar.gz`
- Mode: `compose`

## Checks

| Check | Status | Detail |
|---|---:|---|
| `all_recorded_commands_pass` | FAIL | one or more recorded commands failed |
| `logstore_relaxed_fsync_faster_than_always` | WARN | logstore benchmark policy data is missing |
| `locality_cache_hit_not_worse_than_resource_only` | WARN | locality benchmark did not run |
| `checkpoint_warm_cache_hit` | WARN | warm checkpoint cache-hit proof is missing or false |
| `actor_snapshot_replay_less_than_full` | WARN | actor replay ablation data is missing |
| `dashboard_has_three_workers` | FAIL | dashboard has fewer than three workers |

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 1 | `python_venv_create.log` |
| `python_pip_install` | PASS | 2 | `python_pip_install.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 1 | `build_logservectl.log` |
| `compose_build` | FAIL | 60 | `compose_build.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | PASS | 0 | `package.log` |

## Notes

- verdict is not pass; inspect checks and failed command logs before using the numbers in a report.

## Raw Files

- `bin`
- `build_logservectl.log`
- `command_status.jsonl`
- `compose.env`
- `compose_build.log`
- `environment.txt`
- `experiment-package.tar.gz`
- `package.log`
- `python_compileall.log`
- `python_grpc_deps.log`
- `python_pip_install.log`
- `python_unittest.log`
- `python_venv_create.log`
- `summarize_experiment.log`
- `summary.json`
- `summary.md`
