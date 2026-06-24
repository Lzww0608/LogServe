# Ubuntu Project Acceptance Test

This is the top-level single-server acceptance runner for LogServe. It is meant
for one Ubuntu machine. Docker Compose is used to simulate the multi-process
runtime with logd, control, workers, PostgreSQL, NATS, and MinIO on the same
host.

## Prerequisites

Install the basic tools:

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3 python3-venv tar docker.io docker-compose-plugin
sudo usermod -aG docker "$USER"
```

Log out and log back in after adding your user to the `docker` group, or run the
script with a shell that can reach the Docker daemon.

The runner expects these commands to be available:

- `go`
- `python3`
- `bash`
- `git`
- `tar`
- `docker` and either `docker compose` or `docker-compose`

## Full Run

From the repository root on the Ubuntu server:

```bash
bash scripts/ubuntu_project_acceptance.sh
```

By default this runs:

- prerequisite and server environment capture
- Python virtualenv setup and SDK dependency install
- `go test -count=1 ./...`
- `go vet ./...`
- physical compaction focused tests
- physical compaction race tests for `internal/logstore`
- race tests for `internal/control`, `internal/metadata`, and `internal/worker`
- Python script tests and SDK tests
- Python compile checks
- Docker Compose experiment via `scripts/run_experiment.sh`
- metadata checkpoint acceptance via `scripts/ubuntu_checkpoint_acceptance.sh`
- PostgreSQL async materializer acceptance via `scripts/ubuntu_postgres_async_acceptance.sh`
- automatic project-level summary and result packaging

## Faster Smoke Run

Use this when you are only checking whether the server environment is wired
correctly. It skips the compose-heavy sub-suites but still runs baseline Go,
Python, and physical compaction checks:

```bash
LOGSERVE_PROJECT_RUN_COMPOSE=0 \
LOGSERVE_PROJECT_RUN_CHECKPOINT=0 \
LOGSERVE_PROJECT_RUN_POSTGRES_ASYNC=0 \
bash scripts/ubuntu_project_acceptance.sh
```

## Common Controls

Disable only the PostgreSQL async comparison if the server is small:

```bash
LOGSERVE_PROJECT_RUN_POSTGRES_ASYNC=0 bash scripts/ubuntu_project_acceptance.sh
```

Keep the broad Compose experiment but skip its benchmark runtime:

```bash
LOGSERVE_RUN_BENCHMARK=0 bash scripts/ubuntu_project_acceptance.sh
```

Use the system Python instead of creating a virtualenv:

```bash
LOGSERVE_USE_VENV=0 bash scripts/ubuntu_project_acceptance.sh
```

## Outputs

The runner creates:

```text
reports/ubuntu-project-<timestamp>/
```

Important files:

- `acceptance_summary.md`: top-level human-readable result
- `acceptance_summary.json`: top-level machine-readable result
- `command_status.jsonl`: every command with exit code and duration
- `server_environment.txt`: Ubuntu, Go, Python, Docker, and git snapshot
- `compose_experiment/summary.md`: broad Docker Compose experiment summary
- `checkpoint_acceptance/acceptance_summary.md`: checkpoint acceptance summary
- `postgres_async_acceptance/acceptance_summary.md`: PostgreSQL async acceptance summary
- `ubuntu-project-acceptance-package.tar.gz`: packaged logs and summaries

Send back `acceptance_summary.md`, `acceptance_summary.json`, and
`command_status.jsonl`. If anything fails, also send
`ubuntu-project-acceptance-package.tar.gz`.

## Expected Result

For a full run, the top-level verdict should be `PASS`, and these checks should
be `pass`:

- `go_baseline_tests`
- `physical_compaction_tests`
- `logstore_race_tests`
- `python_script_tests`
- `python_compileall`
- `compose_experiment_pass`
- `checkpoint_acceptance_pass`
- `postgres_async_acceptance_pass`

A passing result means the single-node mechanism validation is healthy: core Go
packages pass, physical compaction preserves log replay across delete/copy/crash
windows, the single-host Docker Compose runtime starts, checkpoint bootstrap is
consistent, and the PostgreSQL async materializer acceptance checks pass. It is
still a single-server validation and should not be read as a production
multi-node performance claim.

## Accepted Project Result (2026-06-24)

The full project acceptance run completed successfully on the Ubuntu server:

```text
Result directory: /home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T025707Z
Verdict: PASS
Package: /home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T025707Z/ubuntu-project-acceptance-package.tar.gz
```

Top-level command status:

| Command | Status | Seconds |
|---|---:|---:|
| `go_test_all` | PASS | 27 |
| `go_vet` | PASS | 0 |
| `go_test_physical_compaction` | PASS | 1 |
| `go_race_logstore` | PASS | 1 |
| `go_race_core` | PASS | 3 |
| `python_script_tests` | PASS | 0 |
| `python_sdk_tests` | PASS | 0 |
| `python_compileall` | PASS | 0 |
| `compose_experiment` | PASS | 80 |
| `checkpoint_acceptance` | PASS | 2 |
| `postgres_async_acceptance` | PASS | 116 |

Accepted checks:

- `go_baseline_tests`: pass
- `physical_compaction_tests`: pass
- `logstore_race_tests`: pass
- `python_script_tests`: pass
- `python_compileall`: pass
- `compose_experiment_pass`: pass
- `checkpoint_acceptance_pass`: pass
- `postgres_async_acceptance_pass`: pass

Key evidence from this run:

- Compose experiment passed with 3 workers and dashboard replay consistency.
- Actor snapshot replay read 1 command versus 21 commands for full/no-snapshot replay.
- The run reported 45 compactable actor-log records and 18,382 compactable bytes.
- The checkpoint acceptance sub-suite read 71 records with checkpoint-plus-tail replay versus 614 with full replay, with `consistent=true` across 156 checked objects.
- PostgreSQL async materialization reduced transaction rate from 72.382 tx/s to 1.304 tx/s and row-write rate from 100.519 rows/s to 16.570 rows/s while keeping task throughput and p99 within acceptance tolerance.
- Physical compaction focused tests and logstore race tests passed in the same top-level run.

Interpret this as the accepted single-server project gate for the current implementation. It validates mechanism correctness and non-regression on one Ubuntu host; it does not claim production multi-node performance or real GPU/vLLM cold-start behavior.
