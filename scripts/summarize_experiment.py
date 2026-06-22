#!/usr/bin/env python3
import json
import re
import sys
from pathlib import Path


BENCH_LINE = re.compile(r"^(Benchmark\S+)\s+\d+\s+(.+)$")


def read_json(path):
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8-sig"))
    except json.JSONDecodeError:
        return None


def read_text(path):
    if not path.exists():
        return ""
    return path.read_text(encoding="utf-8-sig", errors="replace")


def read_statuses(run_dir):
    path = run_dir / "command_status.jsonl"
    if not path.exists():
        return []
    out = []
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            out.append({"name": "malformed_status_line", "exit_code": 1, "duration_sec": 0, "log": ""})
    return out


def read_environment(run_dir):
    out = {}
    for line in read_text(run_dir / "environment.txt").splitlines():
        if "=" in line and not line.startswith(" "):
            key, value = line.split("=", 1)
            out[key.strip()] = value.strip()
    return out


def status_label(code):
    return "PASS" if code == 0 else "FAIL"


def pct(value):
    if value is None:
        return "n/a"
    if isinstance(value, float):
        return f"{value:.3f}"
    return str(value)


def pick_present(data, *keys):
    if not isinstance(data, dict):
        return None
    for key in keys:
        if key in data:
            return data[key]
    return None


def as_number(value):
    if value is None:
        return None
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, (int, float)):
        return value
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def check(name, status, detail, actual=None, expected=None):
    item = {"name": name, "status": status, "detail": detail}
    if actual is not None:
        item["actual"] = actual
    if expected is not None:
        item["expected"] = expected
    return item


def parse_go_benchmarks(path):
    out = []
    for raw in read_text(path).splitlines():
        match = BENCH_LINE.match(raw.strip())
        if not match:
            continue
        name, rest = match.groups()
        tokens = rest.split()
        metrics = {}
        i = 0
        while i + 1 < len(tokens):
            try:
                value = float(tokens[i])
            except ValueError:
                i += 1
                continue
            unit = tokens[i + 1]
            if unit == "ns/op":
                metrics["ns_per_op"] = value
            elif unit == "B/op":
                metrics["bytes_per_op"] = value
            elif unit == "allocs/op":
                metrics["allocs_per_op"] = value
            elif unit.endswith("-ns") or unit.endswith("-ms") or unit.endswith("-rate"):
                metrics[unit.replace("-", "_")] = value
            i += 2
        out.append({"name": name, "metrics": metrics, "raw": raw.strip()})
    return out


def benchmark_highlights(data):
    if not isinstance(data, dict):
        return {}, []
    highlights = {}
    notes = []

    workflow = data.get("workflow_latency") or {}
    if workflow:
        highlights["workflow_p95_ms"] = workflow.get("p95_ms")
        highlights["workflow_p99_ms"] = workflow.get("p99_ms")

    task = data.get("task_throughput") or {}
    if task:
        highlights["task_throughput_tps"] = task.get("throughput_tps")
        highlights["task_p99_latency_ms"] = task.get("p99_latency_ms")

    actor = data.get("actor_recovery_snapshot_ablation") or {}
    snap = actor.get("snapshot_enabled") or {}
    no_snap = actor.get("snapshot_disabled") or {}
    if snap or no_snap:
        highlights["actor_snapshot_replay_commands"] = snap.get("snapshot_replay_commands")
        highlights["actor_trimmed_replay_commands"] = snap.get("trimmed_replay_commands")
        highlights["actor_full_replay_commands"] = snap.get("full_replay_commands")
        highlights["actor_no_snapshot_replay_commands"] = no_snap.get("snapshot_replay_commands")
        highlights["actor_compactable_log_records"] = actor.get("compactable_log_records")
        highlights["actor_compactable_log_bytes"] = actor.get("compactable_log_bytes")

    llm = data.get("llm_cold_start") or {}
    cold = llm.get("cold") or {}
    warm = llm.get("warm") or {}
    if cold or warm:
        highlights["llm_cold_total_latency_ms"] = cold.get("total_latency_ms")
        highlights["llm_warm_total_latency_ms"] = warm.get("total_latency_ms")
        highlights["llm_warm_cache_hit"] = warm.get("cache_hit")
        highlights["llm_cold_checkpoint_fetch_ms"] = cold.get("checkpoint_fetch_ms")
        highlights["llm_warm_checkpoint_fetch_ms"] = warm.get("checkpoint_fetch_ms")

    locality = data.get("locality_ablation") or {}
    resource = locality.get("resource_only") or {}
    aware = locality.get("locality_aware") or {}
    predicted = locality.get("predicted_latency") or {}
    if resource or aware or predicted:
        highlights["resource_only_cache_hit_rate"] = resource.get("cache_hit_rate")
        highlights["locality_aware_cache_hit_rate"] = aware.get("cache_hit_rate")
        highlights["predicted_latency_cache_hit_rate"] = predicted.get("cache_hit_rate")
        highlights["resource_only_p95_latency_ms"] = resource.get("p95_latency_ms")
        highlights["locality_aware_p95_latency_ms"] = aware.get("p95_latency_ms")
        highlights["predicted_latency_p95_latency_ms"] = predicted.get("p95_latency_ms")
        if aware.get("cache_hit_rate") is not None and resource.get("cache_hit_rate") is not None:
            if aware["cache_hit_rate"] < resource["cache_hit_rate"]:
                notes.append("locality-aware cache hit rate was lower than resource-only; inspect worker placement and workload size.")
    return highlights, notes


def logstore_highlights(data):
    if not isinstance(data, dict):
        return {}
    if "policies" not in data or not isinstance(data["policies"], list):
        return data
    out = {}
    for item in data["policies"]:
        if not isinstance(item, dict):
            continue
        policy = item.get("policy", "unknown")
        out[f"logstore_{policy}_append_tps"] = item.get("append_records_sec")
        out[f"logstore_{policy}_read_tps"] = item.get("read_records_sec")
        out[f"logstore_{policy}_recover_ms"] = item.get("recover_ms")
        out[f"logstore_{policy}_segments"] = item.get("segment_count")
    return out


def checkpoint_highlights(data):
    if not isinstance(data, dict):
        return {}
    cold = data.get("cold") or {}
    warm = data.get("warm") or {}
    return {
        "checkpoint_model": data.get("model"),
        "checkpoint_cold_cache_hit": cold.get("cache_hit"),
        "checkpoint_warm_cache_hit": warm.get("cache_hit"),
        "checkpoint_cold_worker_id": cold.get("worker_id"),
        "checkpoint_warm_worker_id": warm.get("worker_id"),
        "checkpoint_cold_fetch_ms": cold.get("checkpoint_fetch_ms"),
        "checkpoint_warm_fetch_ms": warm.get("checkpoint_fetch_ms"),
        "checkpoint_cache_used_bytes": warm.get("cache_used_bytes") or cold.get("cache_used_bytes"),
        "checkpoint_cache_capacity_bytes": warm.get("cache_capacity_bytes") or cold.get("cache_capacity_bytes"),
        "checkpoint_validation_errors": data.get("validation_errors") or [],
    }


def dashboard_highlights(data):
    if not isinstance(data, dict):
        return {}
    return {
        "dashboard_queue_depth": pick_present(data, "queue_depth", "queueDepth"),
        "dashboard_tasks": len(data.get("tasks") or []),
        "dashboard_workflows": len(data.get("workflows") or []),
        "dashboard_actors": len(data.get("actors") or []),
        "dashboard_workers": len(data.get("workers") or []),
        "dashboard_models": len(data.get("models") or []),
        "dashboard_compactable_log_records": pick_present(data, "compactable_log_records", "compactableLogRecords"),
        "dashboard_compactable_log_bytes": pick_present(data, "compactable_log_bytes", "compactableLogBytes"),
    }


def build_checks(statuses, benchmark, logstore, checkpoint, dashboard):
    checks = []
    failed_commands = [item.get("name") for item in statuses if int(item.get("exit_code", 1)) != 0]
    if failed_commands:
        checks.append(check("all_recorded_commands_pass", "fail", "one or more recorded commands failed", failed_commands, "no failed commands"))
    else:
        checks.append(check("all_recorded_commands_pass", "pass", "all recorded commands passed"))

    policies = {}
    if isinstance(logstore, dict):
        for item in logstore.get("policies") or []:
            if isinstance(item, dict) and item.get("policy"):
                policies[item["policy"]] = as_number(item.get("append_records_sec"))
    always = policies.get("always")
    batch = policies.get("batch")
    interval = policies.get("interval")
    if always and batch and interval:
        if batch > always and interval > always:
            checks.append(check("logstore_relaxed_fsync_faster_than_always", "pass", "batch and interval append throughput are greater than always", {"always": always, "batch": batch, "interval": interval}, "batch > always and interval > always"))
        else:
            checks.append(check("logstore_relaxed_fsync_faster_than_always", "warn", "relaxed fsync policies did not both beat always in this run", {"always": always, "batch": batch, "interval": interval}, "batch > always and interval > always"))
    else:
        checks.append(check("logstore_relaxed_fsync_faster_than_always", "warn", "logstore benchmark policy data is missing", policies, "always, batch, interval"))

    locality = benchmark.get("locality_ablation") if isinstance(benchmark, dict) else None
    if isinstance(locality, dict):
        resource_hit = as_number((locality.get("resource_only") or {}).get("cache_hit_rate"))
        aware_hit = as_number((locality.get("locality_aware") or {}).get("cache_hit_rate"))
        if resource_hit is not None and aware_hit is not None:
            status = "pass" if aware_hit >= resource_hit else "warn"
            checks.append(check("locality_cache_hit_not_worse_than_resource_only", status, "locality-aware cache hit rate should not be lower than resource-only", {"resource_only": resource_hit, "locality_aware": aware_hit}, "locality_aware >= resource_only"))
        else:
            checks.append(check("locality_cache_hit_not_worse_than_resource_only", "warn", "locality benchmark cache-hit data is missing"))
    else:
        checks.append(check("locality_cache_hit_not_worse_than_resource_only", "warn", "locality benchmark did not run"))

    checkpoint_errors = []
    warm_hit = None
    if isinstance(checkpoint, dict):
        checkpoint_errors = checkpoint.get("validation_errors") or []
        warm_hit = (checkpoint.get("warm") or {}).get("cache_hit")
    if checkpoint_errors:
        checks.append(check("checkpoint_warm_cache_hit", "fail", "checkpoint cache probe reported validation errors", checkpoint_errors, "no validation errors"))
    elif warm_hit is True:
        checks.append(check("checkpoint_warm_cache_hit", "pass", "warm checkpoint request hit cache"))
    else:
        checks.append(check("checkpoint_warm_cache_hit", "warn", "warm checkpoint cache-hit proof is missing or false", warm_hit, True))

    actor = benchmark.get("actor_recovery_snapshot_ablation") if isinstance(benchmark, dict) else None
    snap = (actor or {}).get("snapshot_enabled") or {}
    snapshot_replay = as_number(snap.get("snapshot_replay_commands"))
    full_replay = as_number(snap.get("full_replay_commands"))
    if snapshot_replay is not None and full_replay is not None:
        status = "pass" if snapshot_replay < full_replay else "warn"
        checks.append(check("actor_snapshot_replay_less_than_full", status, "snapshot replay should use fewer commands than full replay", {"snapshot": snapshot_replay, "full": full_replay}, "snapshot < full"))
    else:
        checks.append(check("actor_snapshot_replay_less_than_full", "warn", "actor replay ablation data is missing"))

    workers = None
    if isinstance(dashboard, dict):
        workers = len(dashboard.get("workers") or [])
    if workers is None:
        checks.append(check("dashboard_has_three_workers", "warn", "dashboard snapshot is missing", workers, ">= 3"))
    elif workers >= 3:
        checks.append(check("dashboard_has_three_workers", "pass", "dashboard contains at least three workers", workers, ">= 3"))
    else:
        checks.append(check("dashboard_has_three_workers", "fail", "dashboard has fewer than three workers", workers, ">= 3"))

    return checks


def verdict_from_checks(checks):
    if any(item["status"] == "fail" for item in checks):
        return "fail"
    if any(item["status"] == "warn" for item in checks):
        return "warn"
    return "pass"


def write_summary(run_dir, environment, statuses, benchmark, logstore, fault, dashboard, checkpoint, go_benchmarks):
    benchmark_summary, notes = benchmark_highlights(benchmark)
    logstore_summary = logstore_highlights(logstore)
    checkpoint_summary = checkpoint_highlights(checkpoint)
    dashboard_summary = dashboard_highlights(dashboard)
    checks = build_checks(statuses, benchmark or {}, logstore or {}, checkpoint or {}, dashboard or {})
    verdict = verdict_from_checks(checks)
    if verdict != "pass":
        notes.append("verdict is not pass; inspect checks and failed command logs before using the numbers in a report.")

    package_path = run_dir / "experiment-package.tar.gz"
    summary = {
        "verdict": verdict,
        "checks": checks,
        "run_dir": str(run_dir),
        "package": str(package_path),
        "environment": environment,
        "commands": statuses,
        "benchmark": benchmark_summary,
        "logstore": logstore_summary,
        "checkpoint_cache": checkpoint_summary,
        "fault_injection": fault or {},
        "dashboard": dashboard_summary,
        "go_benchmarks": go_benchmarks,
        "notes": notes,
    }
    (run_dir / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")

    lines = ["# LogServe Experiment Summary", ""]
    lines.append(f"- Verdict: **{verdict.upper()}**")
    lines.append(f"- Run directory: `{run_dir}`")
    lines.append(f"- Package: `{package_path}`")
    if environment.get("mode"):
        lines.append(f"- Mode: `{environment['mode']}`")
    lines.append("")

    lines.append("## Checks")
    lines.append("")
    lines.append("| Check | Status | Detail |")
    lines.append("|---|---:|---|")
    for item in checks:
        lines.append(f"| `{item['name']}` | {item['status'].upper()} | {item.get('detail', '')} |")

    lines.append("")
    lines.append("## Command Results")
    lines.append("")
    lines.append("| Command | Status | Seconds | Log |")
    lines.append("|---|---:|---:|---|")
    for item in statuses:
        lines.append(
            f"| `{item.get('name')}` | {status_label(int(item.get('exit_code', 1)))} | "
            f"{item.get('duration_sec', 0)} | `{item.get('log', '')}` |"
        )

    sections = [
        ("Benchmark", benchmark_summary),
        ("Logstore Benchmark", logstore_summary),
        ("Checkpoint Cache Probe", checkpoint_summary),
        ("Fault Injection", fault or {}),
        ("Dashboard Snapshot", dashboard_summary),
    ]
    for title, values in sections:
        if not values:
            continue
        lines.append("")
        lines.append(f"## {title}")
        lines.append("")
        for key, value in values.items():
            lines.append(f"- `{key}`: {pct(value)}")

    if go_benchmarks:
        lines.append("")
        lines.append("## Go Benchmarks")
        lines.append("")
        for source, benches in go_benchmarks.items():
            lines.append(f"### {source}")
            for bench in benches[:20]:
                metrics = ", ".join(f"{key}={pct(value)}" for key, value in bench["metrics"].items())
                lines.append(f"- `{bench['name']}`: {metrics}")

    if notes:
        lines.append("")
        lines.append("## Notes")
        lines.append("")
        for note in notes:
            lines.append(f"- {note}")

    lines.append("")
    lines.append("## Raw Files")
    lines.append("")
    skip = {"venv", "runtime"}
    for path in sorted(run_dir.iterdir(), key=lambda p: p.name):
        if path.name in skip:
            continue
        lines.append(f"- `{path.name}`")

    (run_dir / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    return summary


def main():
    if len(sys.argv) != 2:
        print("usage: summarize_experiment.py <run-dir>", file=sys.stderr)
        return 2
    run_dir = Path(sys.argv[1]).resolve()
    statuses = read_statuses(run_dir)
    environment = read_environment(run_dir)
    benchmark = read_json(run_dir / "benchmark.json")
    logstore = read_json(run_dir / "logstore_latest.json")
    fault = read_json(run_dir / "fault_injection.json")
    dashboard = read_json(run_dir / "dashboard_snapshot.json")
    checkpoint = read_json(run_dir / "checkpoint_cache_probe.json")
    go_benchmarks = {
        "scheduler_benchmark.log": parse_go_benchmarks(run_dir / "scheduler_benchmark.log"),
        "metadata_benchmark.log": parse_go_benchmarks(run_dir / "metadata_benchmark.log"),
    }
    go_benchmarks = {key: value for key, value in go_benchmarks.items() if value}
    write_summary(run_dir, environment, statuses, benchmark, logstore, fault, dashboard, checkpoint, go_benchmarks)
    print(run_dir / "summary.md")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
