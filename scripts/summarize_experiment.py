#!/usr/bin/env python3
import json
import sys
from pathlib import Path


def read_json(path):
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8-sig"))
    except json.JSONDecodeError:
        return None


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


def status_label(code):
    return "PASS" if code == 0 else "FAIL"


def pct(value):
    if value is None:
        return "n/a"
    return f"{value:.3f}" if isinstance(value, float) else str(value)


def phase5_highlights(data):
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
        highlights["actor_full_replay_commands"] = snap.get("full_replay_commands")
        highlights["actor_no_snapshot_replay_commands"] = no_snap.get("snapshot_replay_commands")

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
                notes.append("locality-aware cache hit rate was lower than resource-only in this run; inspect worker logs and workload size.")
    return highlights, notes


def logstore_highlights(data):
    if not isinstance(data, dict):
        return {}
    if "policies" in data and isinstance(data["policies"], list):
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
    return data


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


def pick_present(data, *keys):
    for key in keys:
        if key in data:
            return data[key]
    return None


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
    }


def write_summary(run_dir, statuses, phase5, logstore, fault, dashboard, checkpoint):
    phase5_summary, notes = phase5_highlights(phase5)
    logstore_summary = logstore_highlights(logstore)
    checkpoint_summary = checkpoint_highlights(checkpoint)
    dashboard_summary = dashboard_highlights(dashboard)

    summary = {
        "run_dir": str(run_dir),
        "commands": statuses,
        "phase5": phase5_summary,
        "logstore": logstore_summary,
        "checkpoint_cache": checkpoint_summary,
        "fault_injection": fault or {},
        "dashboard": dashboard_summary,
        "notes": notes,
    }
    (run_dir / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")

    lines = []
    lines.append("# LogServe Experiment Summary")
    lines.append("")
    lines.append(f"- Run directory: `{run_dir}`")
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

    if phase5_summary:
        lines.append("")
        lines.append("## Phase 5 Benchmark")
        lines.append("")
        for key, value in phase5_summary.items():
            lines.append(f"- `{key}`: {pct(value)}")

    if logstore_summary:
        lines.append("")
        lines.append("## Logstore Benchmark")
        lines.append("")
        for key, value in logstore_summary.items():
            lines.append(f"- `{key}`: {pct(value)}")

    if checkpoint_summary:
        lines.append("")
        lines.append("## Checkpoint Cache Probe")
        lines.append("")
        for key, value in checkpoint_summary.items():
            lines.append(f"- `{key}`: {pct(value)}")

    if fault:
        lines.append("")
        lines.append("## Fault Injection")
        lines.append("")
        for key, value in fault.items():
            lines.append(f"- `{key}`: {value}")

    if dashboard_summary:
        lines.append("")
        lines.append("## Dashboard Snapshot")
        lines.append("")
        for key, value in dashboard_summary.items():
            lines.append(f"- `{key}`: {pct(value)}")

    if notes:
        lines.append("")
        lines.append("## Notes")
        lines.append("")
        for note in notes:
            lines.append(f"- {note}")

    lines.append("")
    lines.append("## Raw Files")
    lines.append("")
    for name in [
        "environment.txt",
        "command_status.jsonl",
        "phase5_benchmark.json",
        "checkpoint_cache_probe.json",
        "checkpoint_cache_artifact.log",
        "logstore_v1_latest.json",
        "fault_injection.json",
        "dashboard_snapshot.json",
        "summary.json",
    ]:
        if (run_dir / name).exists():
            lines.append(f"- `{name}`")

    (run_dir / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    return summary


def main():
    if len(sys.argv) != 2:
        print("usage: summarize_experiment.py <run-dir>", file=sys.stderr)
        return 2
    run_dir = Path(sys.argv[1]).resolve()
    statuses = read_statuses(run_dir)
    phase5 = read_json(run_dir / "phase5_benchmark.json")
    logstore = read_json(run_dir / "logstore_v1_latest.json")
    fault = read_json(run_dir / "fault_injection.json")
    dashboard = read_json(run_dir / "dashboard_snapshot.json")
    checkpoint = read_json(run_dir / "checkpoint_cache_probe.json")
    write_summary(run_dir, statuses, phase5, logstore, fault, dashboard, checkpoint)
    print(run_dir / "summary.md")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
