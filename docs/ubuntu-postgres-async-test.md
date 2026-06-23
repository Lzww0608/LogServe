# Ubuntu PostgreSQL Async Acceptance Test

This run is intended for one Ubuntu server. Docker Compose starts PostgreSQL,
MinIO, logd, control, and three workers on the same host, then runs sync and
async PostgreSQL metadata materialization back to back.

## Prerequisites

Install these on the server:

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3 python3-venv
```

Install Go and Docker using your normal server process. The script requires:

- `go`
- `python3`
- `docker`
- either `docker compose` or `docker-compose`

Confirm Docker is usable by the current user:

```bash
docker info
docker compose version || docker-compose --version
```

## Run

From the repository root on the Ubuntu server:

```bash
bash scripts/ubuntu_postgres_async_acceptance.sh
```

The wrapper runs:

- prerequisite capture and environment snapshot
- `go test -count=1 ./...`
- `go test -race -count=1 ./internal/metadata ./internal/control`
- Python SDK unittest and compile checks
- Docker Compose sync PostgreSQL metadata run
- Docker Compose async PostgreSQL metadata run
- automatic comparison and result packaging

For a faster smoke run while checking server setup:

```bash
LOGSERVE_SERVER_SKIP_BASELINE=1 \
LOGSERVE_COMPARE_BENCH_TASKS=16 \
LOGSERVE_COMPARE_BENCH_WORKFLOWS=2 \
LOGSERVE_COMPARE_BENCH_LLM_REQUESTS=4 \
LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS=10 \
bash scripts/ubuntu_postgres_async_acceptance.sh
```

For a stronger final run, increase the workload:

```bash
LOGSERVE_COMPARE_BENCH_TASKS=128 \
LOGSERVE_COMPARE_BENCH_WORKFLOWS=10 \
LOGSERVE_COMPARE_BENCH_LLM_REQUESTS=20 \
LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS=100 \
bash scripts/ubuntu_postgres_async_acceptance.sh
```

## Outputs

The wrapper creates:

```text
reports/ubuntu-postgres-async-<timestamp>/
```

Important files:

- `acceptance_summary.md`: top-level human-readable result
- `acceptance_summary.json`: top-level machine-readable result
- `postgres_async_compare/summary.md`: sync-vs-async comparison table
- `postgres_async_compare/comparison.json`: acceptance checks and ratios
- `ubuntu-acceptance-package.tar.gz`: packaged logs and JSON summaries

Send back `acceptance_summary.md`, `acceptance_summary.json`,
`postgres_async_compare/summary.md`, and
`postgres_async_compare/comparison.json`. If a command fails, also send
`ubuntu-acceptance-package.tar.gz`.

## Expected Result

The final comparison is expected to show:

- async task throughput within the configured non-regression tolerance (`LOGSERVE_COMPARE_TASK_THROUGHPUT_MIN_RATIO`, default `0.99`)
- async task submit p99 within the configured non-regression tolerance (`LOGSERVE_COMPARE_TASK_P99_MAX_RATIO`, default `1.0`)
- async PostgreSQL transaction rate lower than sync
- async PostgreSQL row-write rate lower than sync
- async materializer mode observed in dashboard
- async materializer flush errors equal to zero

The summary still reports the exact async/sync ratios and strict-improvement
observations, so a pass should be described as non-regression unless the reported
ratios show a real improvement. If any of these fail, keep the generated package
and inspect the corresponding command log listed in `acceptance_summary.md`.

## Accepted Comparison Snapshot

The accepted comparison from the Ubuntu single-server run is:

```text
reports/ubuntu-postgres-async-20260623T121546Z/postgres_async_compare
Acceptance: PASS
```

| Metric | Sync | Async | Async/Sync |
|---|---:|---:|---:|
| Task throughput tps | 5.08 | 5.03 | 0.9902 |
| Task submit p99 ms | 209 | 209 | 1.0000 |
| PostgreSQL tx/s | 72.629 | 1.329 | 0.0183 |
| PostgreSQL row writes/s | 101.423 | 17.083 | 0.1684 |

Accepted checks:

- `task_throughput_within_tolerance`: pass
- `task_submit_p99_within_tolerance`: pass
- `postgres_transactions_per_sec_reduced`: pass
- `postgres_row_writes_per_sec_reduced`: pass
- `async_materializer_mode_observed`: pass
- `async_materializer_flush_errors_zero`: pass

Dashboard replay consistency passed for both sync and async runs with 5
workflows, 2 actors, 7 checked objects, and no failures. The async dashboard
reported `pending_deltas=6`, `flush_errors=0`, and
`eventual_lag_estimate_ms=840`.

Interpret this as a successful async materialized-view validation: PostgreSQL
transaction/write rates dropped substantially, while task throughput and p99
remained within the configured non-regression tolerance. This run does not show
strict task throughput or p99 improvement.
