#!/usr/bin/env python3
import json
import sys
from pathlib import Path


SUBSUITES = {
    "compose_experiment": {
        "enabled_key": "run_compose_experiment",
        "summary": ("compose_experiment", "summary.json"),
        "markdown": ("compose_experiment", "summary.md"),
        "pass_values": {"PASS"},
    },
    "checkpoint_acceptance": {
        "enabled_key": "run_checkpoint_acceptance",
        "summary": ("checkpoint_acceptance", "acceptance_summary.json"),
        "markdown": ("checkpoint_acceptance", "acceptance_summary.md"),
        "pass_values": {"PASS"},
    },
    "postgres_async_acceptance": {
        "enabled_key": "run_postgres_async_acceptance",
        "summary": ("postgres_async_acceptance", "acceptance_summary.json"),
        "markdown": ("postgres_async_acceptance", "acceptance_summary.md"),
        "pass_values": {"PASS", "PASS"},
    },
}


def read_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8-sig"))
    except Exception:
        return {}


def read_statuses(path):
    statuses = []
    if not path.exists():
        return statuses
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        try:
            statuses.append(json.loads(line))
        except json.JSONDecodeError:
            statuses.append({"name": "malformed_status_line", "exit_code": 1, "duration_sec": 0, "log": ""})
    return statuses


def status_failed(status):
    try:
        return int(status.get("exit_code", 1)) != 0
    except (TypeError, ValueError):
        return True


def status_pass(statuses, name):
    for item in statuses:
        if item.get("name") == name:
            return not status_failed(item)
    return False


def bool_config(config, key, default=False):
    value = config.get(key, default)
    if isinstance(value, bool):
        return value
    return str(value).lower() in {"1", "true", "yes", "on"}


def normalize_verdict(value):
    if value is None:
        return ""
    return str(value).upper()


def collect_subsuites(root, config):
    result = {}
    for name, spec in SUBSUITES.items():
        enabled = bool_config(config, spec["enabled_key"], False)
        summary_path = root.joinpath(*spec["summary"])
        markdown_path = root.joinpath(*spec["markdown"])
        summary = read_json(summary_path) if summary_path.exists() else {}
        verdict = normalize_verdict(summary.get("verdict"))
        if not enabled:
            state = "SKIPPED"
        elif summary_path.exists() and verdict in spec["pass_values"]:
            state = "PASS"
        else:
            state = "FAIL"
        result[name] = {
            "enabled": enabled,
            "state": state,
            "summary_path": str(summary_path),
            "markdown_path": str(markdown_path),
            "summary": summary,
        }
    return result


def build_checks(statuses, subsuites):
    checks = {
        "go_baseline_tests": status_pass(statuses, "go_test_all"),
        "physical_compaction_tests": status_pass(statuses, "go_test_physical_compaction"),
        "logstore_race_tests": status_pass(statuses, "go_race_logstore"),
        "python_script_tests": status_pass(statuses, "python_script_tests"),
        "python_compileall": status_pass(statuses, "python_compileall"),
    }
    for name, suite in subsuites.items():
        if suite["enabled"]:
            checks[f"{name}_pass"] = suite["state"] == "PASS"
    return checks


def write_summary(root):
    root = Path(root)
    config = read_json(root / "run_config.json")
    statuses = read_statuses(root / "command_status.jsonl")
    failed_commands = [item.get("name") for item in statuses if status_failed(item)]
    subsuites = collect_subsuites(root, config)
    checks = build_checks(statuses, subsuites)
    failed_checks = sorted(name for name, passed in checks.items() if not passed)
    package_path = root / "ubuntu-project-acceptance-package.tar.gz"
    send_back = [
        str(root / "acceptance_summary.md"),
        str(root / "acceptance_summary.json"),
        str(root / "command_status.jsonl"),
        str(root / "server_environment.txt"),
        str(package_path),
    ]
    for suite in subsuites.values():
        markdown_path = Path(suite["markdown_path"])
        summary_path = Path(suite["summary_path"])
        if markdown_path.exists():
            send_back.append(str(markdown_path))
        if summary_path.exists():
            send_back.append(str(summary_path))

    verdict = "PASS" if not failed_commands and not failed_checks else "FAIL"
    summary = {
        "verdict": verdict,
        "result_dir": str(root),
        "package": str(package_path),
        "failed_commands": failed_commands,
        "failed_checks": failed_checks,
        "commands": statuses,
        "checks": checks,
        "subsuites": subsuites,
        "run_config": config,
        "send_back": send_back,
    }
    (root / "acceptance_summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")
    write_markdown(root, summary)
    return summary


def write_markdown(root, summary):
    lines = ["# Ubuntu Project Acceptance Summary", ""]
    lines.append(f"- Verdict: **{summary.get('verdict')}**")
    lines.append(f"- Result directory: `{root}`")
    lines.append(f"- Package: `{summary.get('package')}`")
    lines.append("")
    lines.append("## Acceptance Checks")
    lines.append("")
    for key, passed in sorted((summary.get("checks") or {}).items()):
        lines.append(f"- `{key}`: {'pass' if passed else 'fail'}")
    lines.append("")
    lines.append("## Sub-suites")
    lines.append("")
    lines.append("| Suite | State | Summary |")
    lines.append("|---|---:|---|")
    for name, suite in (summary.get("subsuites") or {}).items():
        summary_path = suite.get("markdown_path") if Path(suite.get("markdown_path", "")).exists() else suite.get("summary_path")
        lines.append(f"| `{name}` | {suite.get('state')} | `{summary_path}` |")
    lines.append("")
    lines.append("## Commands")
    lines.append("")
    lines.append("| Command | Status | Seconds | Log |")
    lines.append("|---|---:|---:|---|")
    for item in summary.get("commands") or []:
        status = "PASS" if not status_failed(item) else "FAIL"
        lines.append(f"| `{item.get('name')}` | {status} | {item.get('duration_sec', 0)} | `{item.get('log', '')}` |")
    if summary.get("failed_commands") or summary.get("failed_checks"):
        lines.append("")
        lines.append("## Failures")
        lines.append("")
        for name in summary.get("failed_commands") or []:
            lines.append(f"- failed command: `{name}`")
        for name in summary.get("failed_checks") or []:
            lines.append(f"- failed check: `{name}`")
    lines.append("")
    lines.append("## Send Back")
    lines.append("")
    for path in summary.get("send_back") or []:
        lines.append(f"- `{path}`")
    (root / "acceptance_summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if len(argv) != 1:
        print("usage: summarize_ubuntu_project_acceptance.py <project-acceptance-dir>", file=sys.stderr)
        return 2
    summary = write_summary(Path(argv[0]))
    print(Path(argv[0]) / "acceptance_summary.md")
    return 0 if summary.get("verdict") == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())