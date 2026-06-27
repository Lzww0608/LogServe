# LogServe Experiment Summary

- Run directory: `/home/lab2439/Work/lzww/LogServe/reports/compose-experiment-legacy-03`

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `go_test_all` | PASS | 26 | `go_test_all.log` |
| `go_vet` | PASS | 0 | `go_vet.log` |
| `go_race_control_worker` | PASS | 2 | `go_race_control_worker.log` |
| `python_unittest` | PASS | 0 | `python_unittest.log` |
| `python_compileall` | PASS | 1 | `python_compileall.log` |
| `python_grpc_deps` | PASS | 0 | `python_grpc_deps.log` |
| `logstore_v1_benchmark` | PASS | 12 | `logstore_v1_benchmark.log` |
| `fault_injection_go_tests` | PASS | 5 | `fault_injection_go_tests.log` |
| `runtime_logd_start` | PASS | 0 | `logd.log` |
| `runtime_control_start` | PASS | 0 | `control.log` |
| `runtime_workers_start` | PASS | 4 | `worker_1.log` |
| `phase5_benchmark` | PASS | 8 | `phase5_benchmark.stderr.log` |
| `checkpoint_cache_probe` | PASS | 0 | `checkpoint_cache_probe.stderr.log` |
| `dashboard_snapshot` | PASS | 0 | `dashboard_snapshot.stderr.log` |
| `summarize_experiment` | PASS | 0 | `summarize_experiment.log` |

## Phase 5 Benchmark

- `workflow_p95_ms`: 318
- `workflow_p99_ms`: 318
- `task_throughput_tps`: 9.490
- `task_p99_latency_ms`: 105
- `actor_snapshot_replay_commands`: 1
- `actor_full_replay_commands`: 21
- `actor_no_snapshot_replay_commands`: 21
- `llm_cold_total_latency_ms`: 17
- `llm_warm_total_latency_ms`: 17
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 0
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 1.000
- `locality_aware_cache_hit_rate`: 1.000
- `predicted_latency_cache_hit_rate`: 1.000
- `resource_only_p95_latency_ms`: 105
- `locality_aware_p95_latency_ms`: 207
- `predicted_latency_p95_latency_ms`: 205

## Logstore Benchmark

- `logstore_always_append_tps`: 1693.360
- `logstore_always_read_tps`: 505909.120
- `logstore_always_recover_ms`: 71
- `logstore_always_segments`: 7
- `logstore_batch_append_tps`: 307735.310
- `logstore_batch_read_tps`: 1051234.875
- `logstore_batch_recover_ms`: 66
- `logstore_batch_segments`: 7
- `logstore_interval_append_tps`: 261894.615
- `logstore_interval_read_tps`: 742044.981
- `logstore_interval_recover_ms`: 60
- `logstore_interval_segments`: 7

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_fetch_ms`: 0
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 0
- `checkpoint_cache_capacity_bytes`: 0

## Fault Injection

- `worker_kill_recovery`: passed
- `queue_redelivery`: passed
- `control_restart_probe`: passed
- `logd_restart_probe`: covered_by_logstore_recovery_and_process_logs
- `source`: go test ./tests/integration fault and recovery subset

## Dashboard Snapshot

- `dashboard_queue_depth`: n/a
- `dashboard_tasks`: 570
- `dashboard_workflows`: 16
- `dashboard_actors`: 6
- `dashboard_workers`: 3
- `dashboard_models`: 3

## Raw Files

- `environment.txt`
- `command_status.jsonl`
- `phase5_benchmark.json`
- `checkpoint_cache_probe.json`
- `logstore_v1_latest.json`
- `fault_injection.json`
- `dashboard_snapshot.json`
- `summary.json`
