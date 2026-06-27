# Ubuntu PostgreSQL Async Acceptance Summary

- Verdict: **PASS**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/ubuntu-acceptance-package.tar.gz`
- Compare summary: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare/summary.md`

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `prerequisite_check` | PASS | 0 | `prerequisite_check.log` |
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `postgres_async_compare` | PASS | 110 | `postgres_async_compare.log` |
| `package_results` | PASS | 0 | `package.log` |

## PostgreSQL Async Comparison

- Acceptance: `PASS`
- `task_throughput_within_tolerance`: pass
- `task_submit_p99_within_tolerance`: pass
- `postgres_transactions_per_sec_reduced`: pass
- `postgres_row_writes_per_sec_reduced`: pass
- `async_materializer_mode_observed`: pass
- `async_materializer_flush_errors_zero`: pass

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/acceptance_summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare/comparison.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/ubuntu-acceptance-package.tar.gz`
