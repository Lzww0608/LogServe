# Ubuntu Metadata Checkpoint Acceptance Test

This run is intended for one Ubuntu server. It validates the control-plane
metadata checkpoint optimization with a single-host in-memory shared-log harness:
the test builds task, workflow, actor, and LLM metadata history, writes a
metadata checkpoint, appends log tails, then compares full replay with
checkpoint-plus-tail replay.

Docker is optional for this acceptance path. If Docker Compose is available, the
wrapper records it in the environment snapshot; if it is not available, the
checkpoint acceptance still runs because the correctness and replay-work checks
use the same Go reducers and bootstrap code as the control plane.

## Prerequisites

Install these on the server:

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates build-essential python3
```

Install Go using your normal server process. The wrapper requires:

- `go`
- `python3`
- `bash`
- `tar`

Docker is optional unless you force it with `LOGSERVE_REQUIRE_DOCKER=1`.

## Run

From the repository root on the Ubuntu server:

```bash
bash scripts/ubuntu_checkpoint_acceptance.sh
```

The wrapper runs:

- prerequisite capture and environment snapshot
- `go test -count=1 ./...`
- checkpoint-focused `go test -race` for `internal/control`
- Python script tests and compile checks
- checkpoint acceptance workload with automatic JSON report
- top-level summary and result packaging

For a faster smoke run while checking server setup:

```bash
LOGSERVE_CHECKPOINT_SKIP_BASELINE=1 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS=40 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS=4 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS=4 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS=12 \
bash scripts/ubuntu_checkpoint_acceptance.sh
```

For a stronger final run, increase the workload:

```bash
LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS=1000 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS=80 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS=80 \
LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS=300 \
bash scripts/ubuntu_checkpoint_acceptance.sh
```

You can also run only the checkpoint acceptance sub-suite:

```bash
bash scripts/checkpoint_acceptance.sh
```

## Outputs

The wrapper creates:

```text
reports/ubuntu-checkpoint-<timestamp>/
```

Important files:

- `acceptance_summary.md`: top-level human-readable result
- `acceptance_summary.json`: top-level machine-readable result
- `checkpoint_acceptance/summary.md`: checkpoint replay comparison table
- `checkpoint_acceptance/summary.json`: checkpoint acceptance checks and ratios
- `checkpoint_acceptance/checkpoint_acceptance.json`: raw Go acceptance report
- `ubuntu-checkpoint-acceptance-package.tar.gz`: packaged logs and JSON summaries

Send back `acceptance_summary.md`, `acceptance_summary.json`,
`checkpoint_acceptance/summary.md`,
`checkpoint_acceptance/summary.json`, and
`checkpoint_acceptance/checkpoint_acceptance.json`. If a command fails, also
send `ubuntu-checkpoint-acceptance-package.tar.gz`.

## Expected Result

The final summary is expected to show:

- `checkpoint_created`: pass
- `checkpoint_replay_consistent`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass
- `checkpoint_retention`: pass

The most important quantitative metric is
`checkpoint_records_over_full`. It should be below `1.0`, meaning checkpoint
bootstrap read fewer log records than full replay. `checkpoint_duration_over_full`
is reported as an observation, but small single-node runs can be noisy; treat it
as supporting evidence, not the only acceptance criterion.

If any check fails, inspect the failed command log listed in
`acceptance_summary.md` and keep the generated package for review.

## Accepted Checkpoint Snapshot

The accepted Ubuntu run is:

```text
Run directory: reports/ubuntu-checkpoint-20260623T154803Z
Summary: reports/ubuntu-checkpoint-20260623T154803Z/checkpoint_acceptance/summary.md
Acceptance: PASS
```

Workload:

| Item | Count |
|---|---:|
| Tasks | 120 |
| Workflows | 12 |
| Actors | 12 |
| LLM streams | 40 |
| Tail events | 68 |

Replay work:

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration | 3.759 ms | 2.327 ms | 0.6190 |

Checkpoint contents:

| Item | Count |
|---|---:|
| Streams | 196 |
| Tasks | 132 |
| Workflows | 12 |
| Actors | 12 |
| LLM stats entries | 2 |
| Consistency checked objects | 156 |

Accepted checks:

- `checkpoint_created`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_replay_consistent`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass
- `checkpoint_retention`: pass

This run shows that metadata checkpoint bootstrap keeps the replayed metadata
consistent while reading far fewer historical log records. Full replay read 614
records; checkpoint-plus-tail replay read 71. The measured duration also dropped
from 3.759 ms to 2.327 ms in this single-host run, but the stronger claim is the
record-scan reduction. `ReadLog` calls only dropped modestly because the restart
path still checks stream tails.
