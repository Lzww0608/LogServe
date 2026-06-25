# Ubuntu Project Acceptance Summary

- Verdict: **FAIL**
- Result directory: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/ubuntu-project-acceptance-package.tar.gz`

## Acceptance Checks

- `checkpoint_acceptance_pass`: pass
- `compose_experiment_pass`: pass
- `go_baseline_tests`: pass
- `logstore_race_tests`: pass
- `physical_compaction_tests`: pass
- `postgres_async_acceptance_pass`: pass
- `python_compileall`: pass
- `python_script_tests`: pass

## Sub-suites

| Suite | State | Summary |
|---|---:|---|
| `compose_experiment` | PASS | `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/compose_experiment/summary.md` |
| `checkpoint_acceptance` | PASS | `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/checkpoint_acceptance/acceptance_summary.md` |
| `postgres_async_acceptance` | PASS | `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/postgres_async_acceptance/acceptance_summary.md` |

## Commands

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `prerequisite_check` | PASS | 0 | `prerequisite_check.log` |
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `go_test_all` | PASS | 27 | `go_test_all.log` |
| `go_vet` | FAIL | 1 | `go_vet.log` |
| `go_test_physical_compaction` | PASS | 1 | `go_test_physical_compaction.log` |
| `go_race_logstore` | PASS | 2 | `go_race_logstore.log` |
| `go_race_core` | PASS | 2 | `go_race_core.log` |
| `python_script_tests` | PASS | 0 | `python_script_tests.log` |
| `python_sdk_tests` | PASS | 0 | `python_sdk_tests.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `compose_experiment` | PASS | 82 | `compose_experiment.log` |
| `checkpoint_acceptance` | PASS | 1 | `checkpoint_acceptance.log` |
| `postgres_async_acceptance` | PASS | 116 | `postgres_async_acceptance.log` |
| `package_results` | PASS | 0 | `package.log` |

## Failures

- failed command: `go_vet`

## Send Back

- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/acceptance_summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/command_status.jsonl`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/server_environment.txt`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/ubuntu-project-acceptance-package.tar.gz`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/compose_experiment/summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/compose_experiment/summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/checkpoint_acceptance/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/checkpoint_acceptance/acceptance_summary.json`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/postgres_async_acceptance/acceptance_summary.md`
- `/home/lab2439/Work/lzww/LogServe/reports/ubuntu-project-20260624T020038Z/postgres_async_acceptance/acceptance_summary.json`
