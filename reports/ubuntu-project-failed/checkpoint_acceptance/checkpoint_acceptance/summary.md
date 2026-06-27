# Metadata Checkpoint Acceptance Summary

- Verdict: **PASS**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-failed/checkpoint_acceptance/checkpoint_acceptance`
- Generated at UTC: `2026-06-24T02:02:41Z`

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
| Duration ms | 10.471 | 6.51 | 0.6217 |
| Seq-1 reads for checkpointed streams | n/a | None | n/a |

## Checkpoint

- `id`: checkpoint-f713852b83b7fe04
- `stream_count`: 196
- `task_count`: 132
- `workflow_count`: 12
- `actor_count`: 12
- `llm_stats_count`: 2

## Consistency

- `consistent`: true
- `checked_count`: 156

## Acceptance Checks

- `checkpoint_created`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_replay_consistent`: pass
- `checkpoint_retention`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `checkpoint_unit_tests` | PASS | 1 | `checkpoint_unit_tests.log` |
| `checkpoint_acceptance_go_test` | PASS | 0 | `checkpoint_acceptance_go_test.log` |

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-failed/checkpoint_acceptance/checkpoint_acceptance/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-failed/checkpoint_acceptance/checkpoint_acceptance/summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-failed/checkpoint_acceptance/checkpoint_acceptance/checkpoint_acceptance.json`
