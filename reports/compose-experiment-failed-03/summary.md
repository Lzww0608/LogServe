# LogServe Experiment Summary

- Verdict: **FAIL**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/compose-experiment-failed-03`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/compose-experiment-failed-03/experiment-package.tar.gz`
- Mode: `compose`

## Checks

| Check | Status | Detail |
|---|---:|---|
| `all_recorded_commands_pass` | FAIL | one or more recorded commands failed |
| `logstore_relaxed_fsync_faster_than_always` | PASS | batch and interval append throughput are greater than always |
| `locality_cache_hit_not_worse_than_resource_only` | WARN | locality benchmark did not run |
| `checkpoint_warm_cache_hit` | WARN | warm checkpoint cache-hit proof is missing or false |
| `actor_snapshot_replay_less_than_full` | WARN | actor replay ablation data is missing |
| `dashboard_has_three_workers` | FAIL | dashboard has fewer than three workers |

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `python_venv_create` | PASS | 2 | `python_venv_create.log` |
| `python_pip_install` | PASS | 3 | `python_pip_install.log` |
| `go_test_all` | FAIL | 1 | `go_test_all.log` |
| `go_vet` | PASS | 0 | `go_vet.log` |
| `go_race_control_metadata_worker` | FAIL | 2 | `go_race_control_metadata_worker.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 0 | `build_logservectl.log` |
| `scheduler_benchmark` | PASS | 5 | `scheduler_benchmark.log` |
| `metadata_benchmark` | PASS | 10 | `metadata_benchmark.log` |
| `logstore_benchmark` | PASS | 12 | `logstore_benchmark.log` |
| `fault_injection_go_tests` | FAIL | 0 | `fault_injection_go_tests.log` |
| `compose_build` | FAIL | 0 | `compose_build.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | FAIL | 0 | `package.log` |

## Logstore Benchmark

- `logstore_always_append_tps`: 1737.798
- `logstore_always_read_tps`: 515191.902
- `logstore_always_recover_ms`: 46
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 395112.451
- `logstore_batch_read_tps`: 827868.470
- `logstore_batch_recover_ms`: 29
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 324622.284
- `logstore_interval_read_tps`: 729747.047
- `logstore_interval_recover_ms`: 29
- `logstore_interval_segments`: 7

## Fault Injection

- `worker_kill_recovery`: failed
- `queue_redelivery`: failed
- `control_restart_probe`: failed
- `logd_restart_probe`: covered_by_logstore_recovery_and_process_logs
- `source`: go test ./tests/integration fault and recovery subset

## Go Benchmarks

### scheduler_benchmark.log
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=1-32`: ns_per_op=607.800, bytes_per_op=105.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=10-32`: ns_per_op=2147.000, bytes_per_op=367.000, allocs_per_op=4.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=100-32`: ns_per_op=18145.000, bytes_per_op=2480.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=1-32`: ns_per_op=487.100, bytes_per_op=128.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=10-32`: ns_per_op=1759.000, bytes_per_op=343.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=100-32`: ns_per_op=15454.000, bytes_per_op=2496.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=1-32`: ns_per_op=476.300, bytes_per_op=151.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=10-32`: ns_per_op=1460.000, bytes_per_op=340.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=100-32`: ns_per_op=4089.000, bytes_per_op=2464.000, allocs_per_op=5.000
### metadata_benchmark.log
- `BenchmarkMemoryStoreConcurrentGetTask/legacy-32`: ns_per_op=64.120, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentGetTask/v2-32`: ns_per_op=28.520, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/legacy-32`: ns_per_op=1431.000, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/v2-32`: ns_per_op=4087.000, bytes_per_op=294.000, allocs_per_op=3.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/legacy-32`: ns_per_op=1461.000, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/v2-32`: ns_per_op=168.400, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/legacy-32`: ns_per_op=9272.000, heartbeat_p99_batch_ns=5356014.000, bytes_per_op=3360.000, allocs_per_op=30.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/v2-32`: ns_per_op=6170.000, heartbeat_p99_batch_ns=22931.000, bytes_per_op=3830.000, allocs_per_op=36.000
- `BenchmarkMemoryStoreActiveWorkers/legacy-32`: ns_per_op=61589.000, bytes_per_op=369536.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreActiveWorkers/v2-32`: ns_per_op=68990.000, bytes_per_op=369536.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreUpdateWorkflow/legacy-32`: ns_per_op=12841.000, bytes_per_op=14068.000, allocs_per_op=45.000
- `BenchmarkMemoryStoreUpdateWorkflow/v2-32`: ns_per_op=20063.000, bytes_per_op=18591.000, allocs_per_op=40.000

## Notes

- verdict is not pass; inspect checks and failed command logs before using the numbers in a report.

## Raw Files

- `bin`
- `build_logservectl.log`
- `command_status.jsonl`
- `compose.env`
- `compose_build.log`
- `environment.txt`
- `experiment-package.tar.gz`
- `fault_injection.json`
- `fault_injection_go_tests.log`
- `go_race_control_metadata_worker.log`
- `go_test_all.log`
- `go_vet.log`
- `logstore_benchmark.log`
- `logstore_latest.json`
- `metadata_benchmark.log`
- `metadata_heap.pprof`
- `metadata_mutex.pprof`
- `package.log`
- `python_compileall.log`
- `python_grpc_deps.log`
- `python_pip_install.log`
- `python_unittest.log`
- `python_venv_create.log`
- `scheduler_benchmark.log`
- `summarize_experiment.log`
- `summary.json`
- `summary.md`
