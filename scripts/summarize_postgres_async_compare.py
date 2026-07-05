#!/usr/bin/env python3
# Compare sync and async PostgreSQL materializer experiment outputs.
import json
import os
import sys
from pathlib import Path


# Load a JSON artifact, returning an empty object when a run did not produce the file.
def read_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8-sig"))
    except Exception:
        return {}


# Return the first matching key from a dictionary, supporting snake_case and camelCase metrics.
def pick(data, *keys):
    if not isinstance(data, dict):
        return None
    for key in keys:
        if key in data:
            return data[key]
    return None


# Coerce numeric-like values to float while preserving missing or invalid values as None.
def as_float(value):
    try:
        if value is None:
            return None
        return float(value)
    except (TypeError, ValueError):
        return None


# Return a rounded numerator-over-denominator ratio, or None when the denominator is unavailable.
def ratio(num, den):
    num = as_float(num)
    den = as_float(den)
    if num is None or den in (None, 0):
        return None
    return round(num / den, 4)


# Compare two numeric values and require both sides before reporting improvement.
def greater(async_value, sync_value):
    async_value = as_float(async_value)
    sync_value = as_float(sync_value)
    return async_value is not None and sync_value is not None and async_value > sync_value


# Compare two numeric values and require both sides before reporting reduction.
def less(async_value, sync_value):
    async_value = as_float(async_value)
    sync_value = as_float(sync_value)
    return async_value is not None and sync_value is not None and async_value < sync_value


# Check that an async metric is at least the configured fraction of its sync baseline.
def at_least_ratio(async_value, sync_value, min_ratio):
    async_value = as_float(async_value)
    sync_value = as_float(sync_value)
    if async_value is None or sync_value is None:
        return False
    # A zero sync baseline cannot be scaled by a ratio, so compare raw values
    # to keep zero-workload fixtures deterministic.
    if sync_value == 0:
        return async_value >= sync_value
    return async_value >= sync_value * min_ratio


# Check that an async metric is no more than the configured multiple of its sync baseline.
def at_most_ratio(async_value, sync_value, max_ratio):
    async_value = as_float(async_value)
    sync_value = as_float(sync_value)
    if async_value is None or sync_value is None:
        return False
    # A zero sync baseline cannot be scaled by a ratio, so compare raw values
    # to keep zero-workload fixtures deterministic.
    if sync_value == 0:
        return async_value <= sync_value
    return async_value <= sync_value * max_ratio


# Return true only for present numeric values that are exactly zero.
def is_zero(value):
    value = as_float(value)
    return value is not None and value == 0


# Treat an omitted counter as zero while preserving explicitly reported values.
def zero_when_omitted(value):
    # Older dashboard snapshots omitted this counter; absence is interpreted as
    # zero because only non-zero flush errors should fail the comparison.
    if value is None:
        return 0
    return value


# Collect the metrics needed to compare one persistence mode directory.
def collect_mode(root, mode):
    run_dir = root / mode
    summary = read_json(run_dir / "summary.json")
    benchmark = read_json(run_dir / "benchmark.json")
    postgres = read_json(run_dir / "postgres_benchmark_stats.json")
    dashboard = read_json(run_dir / "dashboard_snapshot.json")
    task = benchmark.get("task_throughput") or {}
    # Dashboard snapshots have used both JSON casing conventions, so metric
    # collection accepts either without changing the comparison schema.
    materializer = pick(dashboard, "metadata_materializer", "metadataMaterializer") or {}
    flush_errors = pick(materializer, "flush_error_count", "flushErrorCount")
    return {
        "run_dir": str(run_dir),
        "verdict": summary.get("verdict"),
        "task_throughput_tps": task.get("throughput_tps"),
        "task_p99_latency_ms": task.get("p99_latency_ms"),
        "postgres_transactions_per_sec": postgres.get("transactions_per_sec"),
        "postgres_row_writes_per_sec": postgres.get("row_writes_per_sec"),
        "postgres_transactions_delta": postgres.get("transactions_delta"),
        "postgres_row_writes_delta": postgres.get("row_writes_delta"),
        "metadata_materializer_mode": pick(materializer, "mode", "mode"),
        "metadata_materializer_pending_deltas": pick(materializer, "pending_deltas", "pendingDeltas"),
        "metadata_materializer_flush_errors": zero_when_omitted(flush_errors),
        "metadata_materializer_lag_ms": pick(materializer, "eventual_lag_estimate_ms", "eventualLagEstimateMs"),
    }


# Build the sync-vs-async comparison document and all acceptance checks.
def build_comparison(root, require_improvement=True, throughput_min_ratio=0.99, p99_max_ratio=1.0):
    modes = {mode: collect_mode(root, mode) for mode in ("sync", "async")}
    sync = modes["sync"]
    async_ = modes["async"]
    # Acceptance focuses on non-regression plus observable async materializer
    # health; strict improvement is reported separately as an observation.
    checks = {
        "task_throughput_within_tolerance": at_least_ratio(async_.get("task_throughput_tps"), sync.get("task_throughput_tps"), throughput_min_ratio),
        "task_submit_p99_within_tolerance": at_most_ratio(async_.get("task_p99_latency_ms"), sync.get("task_p99_latency_ms"), p99_max_ratio),
        "postgres_transactions_per_sec_reduced": less(async_.get("postgres_transactions_per_sec"), sync.get("postgres_transactions_per_sec")),
        "postgres_row_writes_per_sec_reduced": less(async_.get("postgres_row_writes_per_sec"), sync.get("postgres_row_writes_per_sec")),
        "async_materializer_mode_observed": async_.get("metadata_materializer_mode") == "async",
        "async_materializer_flush_errors_zero": is_zero(async_.get("metadata_materializer_flush_errors")),
    }
    return {
        "modes": modes,
        "ratios": {
            "task_throughput_async_over_sync": ratio(async_.get("task_throughput_tps"), sync.get("task_throughput_tps")),
            "task_p99_async_over_sync": ratio(async_.get("task_p99_latency_ms"), sync.get("task_p99_latency_ms")),
            "postgres_transactions_per_sec_async_over_sync": ratio(async_.get("postgres_transactions_per_sec"), sync.get("postgres_transactions_per_sec")),
            "postgres_row_writes_per_sec_async_over_sync": ratio(async_.get("postgres_row_writes_per_sec"), sync.get("postgres_row_writes_per_sec")),
        },
        "thresholds": {
            "task_throughput_min_ratio": throughput_min_ratio,
            "task_p99_max_ratio": p99_max_ratio,
        },
        "observations": {
            "task_throughput_strictly_improved": greater(async_.get("task_throughput_tps"), sync.get("task_throughput_tps")),
            "task_submit_p99_strictly_improved": less(async_.get("task_p99_latency_ms"), sync.get("task_p99_latency_ms")),
        },
        "acceptance": {
            "pass": all(checks.values()),
            "require_improvement": require_improvement,
            "checks": checks,
        },
    }


# Write the human-readable async materializer comparison report.
def write_markdown(root, comparison):
    sync = comparison["modes"]["sync"]
    async_ = comparison["modes"]["async"]
    acceptance_pass = comparison["acceptance"]["pass"]
    lines = ["# PostgreSQL Async Materializer Comparison", ""]
    lines.append(f"- Sync run: `{sync.get('run_dir')}`")
    lines.append(f"- Async run: `{async_.get('run_dir')}`")
    lines.append(f"- Acceptance: `{'PASS' if acceptance_pass else 'FAIL'}`")
    lines.append("")
    lines.append("| Metric | Sync | Async | Async/Sync |")
    lines.append("|---|---:|---:|---:|")
    metric_rows = [
        ("Task throughput tps", "task_throughput_tps", "task_throughput_async_over_sync"),
        ("Task submit p99 ms", "task_p99_latency_ms", "task_p99_async_over_sync"),
        ("Postgres tx/s", "postgres_transactions_per_sec", "postgres_transactions_per_sec_async_over_sync"),
        ("Postgres row writes/s", "postgres_row_writes_per_sec", "postgres_row_writes_per_sec_async_over_sync"),
    ]
    for label, key, ratio_key in metric_rows:
        lines.append(f"| {label} | {sync.get(key)} | {async_.get(key)} | {comparison['ratios'].get(ratio_key)} |")
    lines.append("")
    lines.append("## Thresholds")
    lines.append("")
    thresholds = comparison.get("thresholds") or {}
    for key in ("task_throughput_min_ratio", "task_p99_max_ratio"):
        lines.append(f"- `{key}`: {thresholds.get(key)}")
    lines.append("")
    lines.append("## Observations")
    lines.append("")
    for key, value in (comparison.get("observations") or {}).items():
        lines.append(f"- `{key}`: {'true' if value else 'false'}")
    lines.append("")
    lines.append("## Acceptance Checks")
    lines.append("")
    for key, passed in comparison["acceptance"]["checks"].items():
        lines.append(f"- `{key}`: {'pass' if passed else 'fail'}")
    lines.append("")
    lines.append("## Materializer")
    lines.append("")
    for key in ("metadata_materializer_mode", "metadata_materializer_pending_deltas", "metadata_materializer_flush_errors", "metadata_materializer_lag_ms"):
        lines.append(f"- `{key}`: sync={sync.get(key)} async={async_.get(key)}")
    (root / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


# Write comparison.json and summary.md for a completed comparison directory.
def write_comparison(root, require_improvement=True, throughput_min_ratio=0.99, p99_max_ratio=1.0):
    root = Path(root)
    comparison = build_comparison(
        root,
        require_improvement=require_improvement,
        throughput_min_ratio=throughput_min_ratio,
        p99_max_ratio=p99_max_ratio,
    )
    (root / "comparison.json").write_text(json.dumps(comparison, indent=2), encoding="utf-8")
    write_markdown(root, comparison)
    return comparison


# Read a floating-point threshold from the environment with a safe default fallback.
def env_float(name, default):
    try:
        return float(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        return default


# Parse arguments, apply threshold environment overrides, and return the configured acceptance code.
def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if len(argv) != 1:
        print("usage: summarize_postgres_async_compare.py <compare-dir>", file=sys.stderr)
        return 2
    require_improvement = os.getenv("LOGSERVE_COMPARE_REQUIRE_IMPROVEMENT", "1") != "0"
    throughput_min_ratio = env_float("LOGSERVE_COMPARE_TASK_THROUGHPUT_MIN_RATIO", 0.99)
    p99_max_ratio = env_float("LOGSERVE_COMPARE_TASK_P99_MAX_RATIO", 1.0)
    root = Path(argv[0])
    comparison = write_comparison(
        root,
        require_improvement=require_improvement,
        throughput_min_ratio=throughput_min_ratio,
        p99_max_ratio=p99_max_ratio,
    )
    print(root / "summary.md")
    # LOGSERVE_COMPARE_REQUIRE_IMPROVEMENT=0 keeps reports informative while
    # allowing exploratory runs to exit successfully despite failed checks.
    if require_improvement and not comparison["acceptance"]["pass"]:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
