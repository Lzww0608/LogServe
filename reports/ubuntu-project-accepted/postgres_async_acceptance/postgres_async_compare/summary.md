# PostgreSQL Async Materializer Comparison

- Sync run: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare/sync`
- Async run: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-accepted/postgres_async_acceptance/postgres_async_compare/async`
- Acceptance: `PASS`

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput tps | 5.0 | 5.0 | 1.0 |
| Task submit p99 ms | 209 | 207 | 0.9904 |
| Postgres tx/s | 72.382 | 1.304 | 0.018 |
| Postgres row writes/s | 100.519 | 16.57 | 0.1648 |

## Thresholds

- `task_throughput_min_ratio`: 0.99
- `task_p99_max_ratio`: 1.0

## Observations

- `task_throughput_strictly_improved`: false
- `task_submit_p99_strictly_improved`: true

## Acceptance Checks

- `task_throughput_within_tolerance`: pass
- `task_submit_p99_within_tolerance`: pass
- `postgres_transactions_per_sec_reduced`: pass
- `postgres_row_writes_per_sec_reduced`: pass
- `async_materializer_mode_observed`: pass
- `async_materializer_flush_errors_zero`: pass

## Materializer

- `metadata_materializer_mode`: sync=sync async=async
- `metadata_materializer_pending_deltas`: sync=None async=6
- `metadata_materializer_flush_errors`: sync=0 async=0
- `metadata_materializer_lag_ms`: sync=None async=746
