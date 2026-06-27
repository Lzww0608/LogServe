# Ubuntu PostgreSQL Async Acceptance Summary

- Verdict: **FAIL**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/ubuntu-acceptance-package.tar.gz`
- Compare summary: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/postgres_async_compare/summary.md`

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `prerequisite_check` | PASS | 0 | `prerequisite_check.log` |
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `go_test_all` | PASS | 29 | `go_test_all.log` |
| `go_race_metadata_control` | PASS | 14 | `go_race_metadata_control.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `postgres_async_compare` | FAIL | 124 | `postgres_async_compare.log` |
| `package_results` | PASS | 0 | `package.log` |

## PostgreSQL Async Comparison

- Acceptance: `FAIL`
- `task_throughput_improved`: fail
- `task_submit_p99_improved`: pass
- `postgres_transactions_per_sec_reduced`: pass
- `postgres_row_writes_per_sec_reduced`: pass
- `async_materializer_mode_observed`: pass
- `async_materializer_flush_errors_zero`: fail

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/acceptance_summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/postgres_async_compare/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/postgres_async_compare/comparison.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-postgres-async-failed-01/ubuntu-acceptance-package.tar.gz`
