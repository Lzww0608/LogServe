# Metadata Checkpoint Acceptance Summary

- Verdict: **FAIL**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance`
- Generated at UTC: `2026-06-23T15:38:53Z`

## Workload

| Item | Count |
|---|---:|
| `tasks` | 120 |
| `workflows` | 12 |
| `actors` | 12 |
| `llm_streams` | 40 |
| `tail_events` | 68 |

## Replay Work

| Metric | Full replay | Checkpoint replay | Checkpoint/Full |
|---|---:|---:|---:|
| Records read | 614 | 71 | 0.1156 |
| ReadLog calls | 224 | 201 | 0.8973 |
| Duration ms | 7.676 | 5.573 | 0.726 |
| Seq-1 reads for checkpointed streams | n/a | None | n/a |

## Checkpoint

- `id`: checkpoint-0d063f68110f243a
- `stream_count`: 196
- `task_count`: 132
- `workflow_count`: 12
- `actor_count`: 12
- `llm_stats_count`: 2

## Consistency

- `consistent`: false
- `checked_count`: 157
- `failure_keys`: llm_stats

## Acceptance Checks

- `checkpoint_created`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_replay_consistent`: fail
- `checkpoint_retention`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `checkpoint_unit_tests` | PASS | 1 | `checkpoint_unit_tests.log` |
| `checkpoint_acceptance_go_test` | FAIL | 0 | `checkpoint_acceptance_go_test.log` |

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/checkpoint_acceptance.json`
