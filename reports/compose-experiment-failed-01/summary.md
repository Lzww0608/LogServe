# LogServe Experiment Summary

- Verdict: **FAIL**
- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/compose-experiment-failed-01`
- Package: `/home/lab2439/Work/lzww/LogServe/reports/compose-experiment-failed-01/experiment-package.tar.gz`
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
| `python_pip_install` | PASS | 4 | `python_pip_install.log` |
| `go_test_all` | FAIL | 2 | `go_test_all.log` |
| `go_vet` | PASS | 2 | `go_vet.log` |
| `go_race_control_metadata_worker` | FAIL | 13 | `go_race_control_metadata_worker.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `build_logservectl` | PASS | 1 | `build_logservectl.log` |
| `scheduler_benchmark` | PASS | 6 | `scheduler_benchmark.log` |
| `metadata_benchmark` | PASS | 10 | `metadata_benchmark.log` |
| `logstore_benchmark` | PASS | 12 | `logstore_benchmark.log` |
| `fault_injection_go_tests` | FAIL | 0 | `fault_injection_go_tests.log` |
| `compose_build` | FAIL | 0 | `compose_build.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |
| `package_results` | FAIL | 0 | `package.log` |

## Logstore Benchmark

- `logstore_always_append_tps`: 1769.699
- `logstore_always_read_tps`: 774469.759
- `logstore_always_recover_ms`: 38
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 286404.746
- `logstore_batch_read_tps`: 547451.764
- `logstore_batch_recover_ms`: 44
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 308212.552
- `logstore_interval_read_tps`: 590943.955
- `logstore_interval_recover_ms`: 35
- `logstore_interval_segments`: 7

## Fault Injection

- `worker_kill_recovery`: failed
- `queue_redelivery`: failed
- `control_restart_probe`: failed
- `logd_restart_probe`: covered_by_logstore_recovery_and_process_logs
- `source`: go test ./tests/integration fault and recovery subset

## Go Benchmarks

### scheduler_benchmark.log
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=1-32`: ns_per_op=477.100, bytes_per_op=105.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=10-32`: ns_per_op=1936.000, bytes_per_op=367.000, allocs_per_op=4.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=1000/workers=100-32`: ns_per_op=17501.000, bytes_per_op=2480.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=1-32`: ns_per_op=422.800, bytes_per_op=128.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=10-32`: ns_per_op=1699.000, bytes_per_op=344.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=10000/workers=100-32`: ns_per_op=15910.000, bytes_per_op=2497.000, allocs_per_op=6.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=1-32`: ns_per_op=506.400, bytes_per_op=151.000, allocs_per_op=2.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=10-32`: ns_per_op=1560.000, bytes_per_op=342.000, allocs_per_op=3.000
- `BenchmarkSchedulerAssignMixedBacklog/depth=100000/workers=100-32`: ns_per_op=3938.000, bytes_per_op=2465.000, allocs_per_op=5.000
### metadata_benchmark.log
- `BenchmarkMemoryStoreConcurrentGetTask/legacy-32`: ns_per_op=61.430, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentGetTask/v2-32`: ns_per_op=28.990, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/legacy-32`: ns_per_op=1096.000, bytes_per_op=0.000, allocs_per_op=0.000
- `BenchmarkMemoryStoreConcurrentLeaseComplete/v2-32`: ns_per_op=2444.000, bytes_per_op=294.000, allocs_per_op=3.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/legacy-32`: ns_per_op=1376.000, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreConcurrentHeartbeat/v2-32`: ns_per_op=167.100, bytes_per_op=560.000, allocs_per_op=5.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/legacy-32`: ns_per_op=6422.000, heartbeat_p99_batch_ns=4353714.000, bytes_per_op=3360.000, allocs_per_op=30.000
- `BenchmarkMemoryStoreHeartbeatUnderCompleteP99/v2-32`: ns_per_op=4487.000, heartbeat_p99_batch_ns=17325.000, bytes_per_op=3808.000, allocs_per_op=36.000
- `BenchmarkMemoryStoreActiveWorkers/legacy-32`: ns_per_op=62333.000, bytes_per_op=369537.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreActiveWorkers/v2-32`: ns_per_op=65684.000, bytes_per_op=369538.000, allocs_per_op=3001.000
- `BenchmarkMemoryStoreUpdateWorkflow/legacy-32`: ns_per_op=8469.000, bytes_per_op=14066.000, allocs_per_op=45.000
- `BenchmarkMemoryStoreUpdateWorkflow/v2-32`: ns_per_op=18624.000, bytes_per_op=18589.000, allocs_per_op=40.000

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
