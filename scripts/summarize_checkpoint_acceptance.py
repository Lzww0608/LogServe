#!/usr/bin/env python3
# Summarize checkpoint acceptance artifacts into JSON and Markdown reports.
import json
import sys
from pathlib import Path


# Load a UTF-8 JSON artifact, returning an empty object when evidence is missing or malformed.
def read_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8-sig"))
    # Acceptance runs may omit optional artifacts after early failure; callers
    # treat an empty object as missing evidence rather than raising here.
    except Exception:
        return {}


# Read command_status.jsonl, preserving malformed lines as failed command entries.
def read_statuses(path):
    statuses = []
    if not path.exists():
        return statuses
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        try:
            statuses.append(json.loads(line))
        # Preserve malformed status lines so the final verdict surfaces log
        # corruption instead of silently dropping it.
        except json.JSONDecodeError:
            statuses.append({"name": "malformed_status_line", "exit_code": 1, "duration_sec": 0, "log": ""})
    return statuses


# Return whether a command status failed, treating invalid exit codes as failures.
def status_failed(status):
    try:
        return int(status.get("exit_code", 1)) != 0
    except (TypeError, ValueError):
        return True


# Render boolean checks using the lowercase pass/fail labels expected in Markdown tables.
def pass_text(value):
    return "pass" if value else "fail"


# Build summary.json from checkpoint acceptance outputs and return the normalized summary.
def write_summary(root):
    root = Path(root)
    acceptance = read_json(root / "checkpoint_acceptance.json")
    statuses = read_statuses(root / "command_status.jsonl")
    failed_commands = [item.get("name") for item in statuses if status_failed(item)]
    checks = acceptance.get("checks") or {}
    failed_checks = sorted(name for name, passed in checks.items() if not passed)
    acceptance_pass = str(acceptance.get("verdict", "")).upper() == "PASS" and not failed_checks
    # Both semantic checkpoint checks and recorded commands must pass;
    # either failure source marks the package as failed.
    verdict = "PASS" if acceptance_pass and not failed_commands else "FAIL"
    summary = {
        "verdict": verdict,
        "result_dir": str(root),
        "failed_commands": failed_commands,
        "failed_checks": failed_checks,
        "commands": statuses,
        "acceptance": acceptance,
        "send_back": [
            str(root / "summary.md"),
            str(root / "summary.json"),
            str(root / "checkpoint_acceptance.json"),
        ],
    }
    (root / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")
    write_markdown(root, summary)
    return summary


# Write the human-readable checkpoint acceptance report from the normalized summary object.
def write_markdown(root, summary):
    acceptance = summary.get("acceptance") or {}
    workload = acceptance.get("workload") or {}
    checkpoint = acceptance.get("checkpoint") or {}
    full = acceptance.get("full_replay") or {}
    fast = acceptance.get("checkpoint_replay") or {}
    ratios = acceptance.get("ratios") or {}
    consistency = acceptance.get("consistency") or {}
    lines = ["# Metadata Checkpoint Acceptance Summary", ""]
    lines.append(f"- Verdict: **{summary.get('verdict')}**")
    lines.append(f"- Result directory: `{root}`")
    if acceptance.get("generated_at_utc"):
        lines.append(f"- Generated at UTC: `{acceptance.get('generated_at_utc')}`")
    lines.append("")
    lines.append("## Workload")
    lines.append("")
    lines.append("| Item | Count |")
    lines.append("|---|---:|")
    for key in ("tasks", "workflows", "actors", "llm_streams", "tail_events"):
        lines.append(f"| `{key}` | {workload.get(key)} |")
    lines.append("")
    lines.append("## Replay Work")
    lines.append("")
    lines.append("| Metric | Full replay | Checkpoint replay | Checkpoint/Full |")
    lines.append("|---|---:|---:|---:|")
    lines.append(f"| Records read | {full.get('records_read')} | {fast.get('records_read')} | {ratios.get('checkpoint_records_over_full')} |")
    lines.append(f"| ReadLog calls | {full.get('read_log_calls')} | {fast.get('read_log_calls')} | {ratios.get('checkpoint_read_calls_over_full')} |")
    lines.append(f"| Duration ms | {full.get('duration_ms')} | {fast.get('duration_ms')} | {ratios.get('checkpoint_duration_over_full')} |")
    lines.append(f"| Seq-1 reads for checkpointed streams | n/a | {fast.get('seq_1_reads_for_checkpointed_streams')} | n/a |")
    lines.append("")
    lines.append("## Checkpoint")
    lines.append("")
    for key in ("id", "stream_count", "task_count", "workflow_count", "actor_count", "llm_stats_count"):
        lines.append(f"- `{key}`: {checkpoint.get(key)}")
    lines.append("")
    lines.append("## Consistency")
    lines.append("")
    lines.append(f"- `consistent`: {str(consistency.get('consistent')).lower()}")
    lines.append(f"- `checked_count`: {consistency.get('checked_count')}")
    if consistency.get("failure_keys"):
        lines.append(f"- `failure_keys`: {', '.join(consistency.get('failure_keys'))}")
    lines.append("")
    lines.append("## Acceptance Checks")
    lines.append("")
    for key, passed in sorted((acceptance.get("checks") or {}).items()):
        lines.append(f"- `{key}`: {pass_text(passed)}")
    if summary.get("commands"):
        lines.append("")
        lines.append("## Commands")
        lines.append("")
        lines.append("| Command | Status | Seconds | Log |")
        lines.append("|---|---:|---:|---|")
        for item in summary.get("commands") or []:
            status = "PASS" if not status_failed(item) else "FAIL"
            lines.append(f"| `{item.get('name')}` | {status} | {item.get('duration_sec', 0)} | `{item.get('log', '')}` |")
    lines.append("")
    lines.append("## Send Back")
    lines.append("")
    for path in summary.get("send_back") or []:
        lines.append(f"- `{path}`")
    (root / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


# Parse the result directory argument, write reports, and exit from the verdict.
def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if len(argv) != 1:
        print("usage: summarize_checkpoint_acceptance.py <checkpoint-acceptance-dir>", file=sys.stderr)
        return 2
    summary = write_summary(Path(argv[0]))
    print(Path(argv[0]) / "summary.md")
    return 0 if summary.get("verdict") == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
