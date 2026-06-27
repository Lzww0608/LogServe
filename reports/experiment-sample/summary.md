# LogServe Experiment Summary

- Run directory: `F:\Code\Go-Programming\LogServe\reports\experiment-sample`

## Command Results

| Command | Status | Seconds | Log |
|---|---:|---:|---|
| `go_test_all` | PASS | 1 | `go_test_all.log` |

## Benchmark

- `workflow_p95_ms`: 10
- `workflow_p99_ms`: 12
- `task_throughput_tps`: 100
- `task_p99_latency_ms`: 3
- `llm_cold_total_latency_ms`: 100
- `llm_warm_total_latency_ms`: 20
- `llm_warm_cache_hit`: True
- `llm_cold_checkpoint_fetch_ms`: 5
- `llm_warm_checkpoint_fetch_ms`: 0
- `resource_only_cache_hit_rate`: 0.100
- `locality_aware_cache_hit_rate`: 0.900
- `predicted_latency_cache_hit_rate`: n/a
- `resource_only_p95_latency_ms`: 100
- `locality_aware_p95_latency_ms`: 40
- `predicted_latency_p95_latency_ms`: n/a

## Logstore Benchmark

- `logstore_always_append_tps`: 123.400
- `logstore_always_read_tps`: 567.800
- `logstore_always_recover_ms`: 9
- `logstore_always_segments`: 2

## Checkpoint Cache Probe

- `checkpoint_model`: model-D
- `checkpoint_cold_cache_hit`: False
- `checkpoint_warm_cache_hit`: True
- `checkpoint_cold_worker_id`: n/a
- `checkpoint_warm_worker_id`: n/a
- `checkpoint_cold_fetch_ms`: 4
- `checkpoint_warm_fetch_ms`: 0
- `checkpoint_cache_used_bytes`: 1048576
- `checkpoint_cache_capacity_bytes`: 16777216
- `checkpoint_validation_errors`: []

## Dashboard Snapshot

- `dashboard_queue_depth`: 0
- `dashboard_tasks`: 1
- `dashboard_workflows`: 0
- `dashboard_actors`: 0
- `dashboard_workers`: 1
- `dashboard_models`: 1
- `dashboard_compactable_log_records`: n/a
- `dashboard_compactable_log_bytes`: n/a

## Raw Files

- `command_status.jsonl`
- `benchmark.json`
- `checkpoint_cache_probe.json`
- `logstore_latest.json`
- `dashboard_snapshot.json`
- `summary.json`
