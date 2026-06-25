# PostgreSQL Async Materializer Comparison

- Sync run: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/postgres_async_acceptance/postgres_async_compare/sync`
- Async run: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/postgres_async_acceptance/postgres_async_compare/async`
- Acceptance: `PASS`

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput tps | 5.01 | 5.0 | 0.998 |
| Task submit p99 ms | 210 | 208 | 0.9905 |
| Postgres tx/s | 71.298 | 1.323 | 0.0186 |
| Postgres row writes/s | 102.558 | 16.947 | 0.1652 |

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
- `metadata_materializer_pending_deltas`: sync=None async=3
- `metadata_materializer_flush_errors`: sync=0 async=0
- `metadata_materializer_lag_ms`: sync=None async=31
