# LogServe Experiment Summary

- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/experiment-20260610T013044794327660Z`

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `go_test_all` | PASS | 26 | `go_test_all.log` |
| `go_vet` | PASS | 1 | `go_vet.log` |
| `go_race_control_worker` | PASS | 2 | `go_race_control_worker.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 0 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `logstore_v1_benchmark` | PASS | 12 | `logstore_v1_benchmark.log` |
| `fault_injection_go_tests` | PASS | 5 | `fault_injection_go_tests.log` |
| `runtime_logd_start` | PASS | 0 | `logd.log` |
| `runtime_control_start` | PASS | 0 | `control.log` |
| `runtime_workers_start` | PASS | 4 | `worker_1.log` |
| `phase5_benchmark` | PASS | 16 | `phase5_benchmark.stderr.log` |
| `checkpoint_cache_probe` | PASS | 1 | `checkpoint_cache_probe.stderr.log` |
| `checkpoint_cache_artifact` | PASS | 0 | `checkpoint_cache_artifact.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |

## Phase 5 Benchmark

- `workflow_p95_ms`: 822
- `workflow_p99_ms`: 822
- `task_throughput_tps`: 4.860
- `task_p99_latency_ms`: 207
- `actor_snapshot_replay_commands`: 1
- `actor_full_replay_commands`: 21
- `actor_no_snapshot_replay_commands`: 21
- `llm_cold_total_latency_ms`: 20
- `llm_warm_total_latency_ms`: 17
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 1
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 0.667
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 204
- `locality_aware_p95_latency_ms`: 206
- `predicted_latency_p95_latency_ms`: 205

## Logstore Benchmark

- `logstore_always_append_tps`: 1674.832
- `logstore_always_read_tps`: 537513.786
- `logstore_always_recover_ms`: 51
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 279350.802
- `logstore_batch_read_tps`: 597123.548
- `logstore_batch_recover_ms`: 66
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 266862.259
- `logstore_interval_read_tps`: 498600.466
- `logstore_interval_recover_ms`: 63
- `logstore_interval_segments`: 7

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_worker_id`: bench-worker-1
- `checkpoint_warm_worker_id`: bench-worker-1
- `checkpoint_cold_fetch_ms`: 1
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 3145728
- `checkpoint_cache_capacity_bytes`: 16777216
- `checkpoint_validation_errors`: []

## Fault Injection

- `worker_kill_recovery`: passed
- `queue_redelivery`: passed
- `control_restart_probe`: passed
- `logd_restart_probe`: covered_by_logstore_recovery_and_process_logs
- `source`: go test ./tests/integration fault and recovery subset

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 84
- `dashboard_workflows`: 3
- `dashboard_actors`: 2
- `dashboard_workers`: 3
- `dashboard_models`: 3

## Raw Files

- `environment.txt`
- `command_status.jsonl`
- `phase5_benchmark.json`
- `checkpoint_cache_probe.json`
- `checkpoint_cache_artifact.log`
- `logstore_v1_latest.json`
- `fault_injection.json`
- `dashboard_snapshot.json`
- `summary.json`
