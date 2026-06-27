# Ubuntu Metadata Checkpoint Acceptance Summary

- Verdict: **FAIL**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/ubuntu-checkpoint-acceptance-package.tar.gz`
- Checkpoint summary: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/summary.md`

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `prerequisite_check` | PASS | 0 | `prerequisite_check.log` |
| `go_test_all` | PASS | 30 | `go_test_all.log` |
| `go_race_control_checkpoint` | PASS | 3 | `go_race_control_checkpoint.log` |
| `python_script_tests` | PASS | 0 | `python_script_tests.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `checkpoint_acceptance` | FAIL | 1 | `checkpoint_acceptance.log` |
| `package_results` | PASS | 0 | `package.log` |

## Checkpoint Acceptance

- Verdict: `FAIL`
- Records read ratio: `0.1156`
- Duration ratio: `0.726`
- `checkpoint_created`: pass
- `checkpoint_read_records_reduced`: pass
- `checkpoint_replay_consistent`: fail
- `checkpoint_retention`: pass
- `checkpoint_tail_only_reads`: pass
- `corrupt_checkpoint_fallback`: pass

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/acceptance_summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/checkpoint_acceptance/checkpoint_acceptance.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-checkpoint-failed/ubuntu-checkpoint-acceptance-package.tar.gz`
