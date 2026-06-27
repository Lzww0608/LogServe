#!/usr/bin/env python3
import json
import sys
from pathlib import Path


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


def build_checks(statuses, config, probe):
    checks = {
        "go_web_tests": status_pass(statuses, "go_test_web"),
        "go_web_vet": status_pass(statuses, "go_vet_web"),
        "web_build": status_pass(statuses, "web_build"),
        "python_script_tests": status_pass(statuses, "python_script_tests"),
    }
    if bool_config(config, "run_npm_ci", True):
        checks["web_npm_ci"] = status_pass(statuses, "web_npm_ci")
    if bool_config(config, "run_docker", True):
        checks["docker_compose_config"] = status_pass(statuses, "docker_compose_config")
        checks["docker_compose_build"] = status_pass(statuses, "docker_compose_build")
        checks["docker_compose_up"] = status_pass(statuses, "docker_compose_up")
        checks["web_health_ready"] = status_pass(statuses, "web_health_ready")
        checks["console_http_probe"] = status_pass(statuses, "console_http_probe") and probe.get("verdict") == "PASS"
        for key, value in sorted((probe.get("checks") or {}).items()):
            checks[f"probe_{key}"] = bool(value)
    return checks


def send_back_paths(root):
    names = [
        "acceptance_summary.md",
        "acceptance_summary.json",
        "command_status.jsonl",
        "server_environment.txt",
        "run_config.json",
        "console_http_probe.json",
        "compose_ps.txt",
        "compose.log",
        "console-acceptance-package.tar.gz",
    ]
    return [str(root / name) for name in names if (root / name).exists() or name == "console-acceptance-package.tar.gz"]


def write_summary(root):
    root = Path(root)
    config = read_json(root / "run_config.json")
    statuses = read_statuses(root / "command_status.jsonl")
    probe = read_json(root / "console_http_probe.json")
    checks = build_checks(statuses, config, probe)
    failed_commands = [item.get("name") for item in statuses if status_failed(item)]
    failed_checks = sorted(name for name, passed in checks.items() if not passed)
    package_path = root / "console-acceptance-package.tar.gz"
    summary = {
        "verdict": "PASS" if not failed_commands and not failed_checks else "FAIL",
        "result_dir": str(root),
        "package": str(package_path),
        "failed_commands": failed_commands,
        "failed_checks": failed_checks,
        "commands": statuses,
        "checks": checks,
        "probe": probe,
        "run_config": config,
        "send_back": send_back_paths(root),
    }
    (root / "acceptance_summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")
    write_markdown(root, summary)
    return summary


def write_markdown(root, summary):
    lines = ["# Ubuntu Console Acceptance Summary", ""]
    lines.append(f"- Verdict: **{summary.get('verdict')}**")
    lines.append(f"- Result directory: `{root}`")
    lines.append(f"- Package: `{summary.get('package')}`")
    lines.append("")
    lines.append("## Acceptance Checks")
    lines.append("")
    for key, passed in sorted((summary.get("checks") or {}).items()):
        lines.append(f"- `{key}`: {'pass' if passed else 'fail'}")
    lines.append("")
    lines.append("## Commands")
    lines.append("")
    lines.append("| Command | Status | Seconds | Log |")
    lines.append("|---|---:|---:|---|")
    for item in summary.get("commands") or []:
        status = "PASS" if not status_failed(item) else "FAIL"
        lines.append(f"| `{item.get('name')}` | {status} | {item.get('duration_sec', 0)} | `{item.get('log', '')}` |")
    probe = summary.get("probe") or {}
    if probe:
        lines.append("")
        lines.append("## HTTP Probe")
        lines.append("")
        lines.append(f"- Probe verdict: **{probe.get('verdict', 'UNKNOWN')}**")
        for key, passed in sorted((probe.get("checks") or {}).items()):
            lines.append(f"- `{key}`: {'pass' if passed else 'fail'}")
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
        print("usage: summarize_console_acceptance.py <console-acceptance-dir>", file=sys.stderr)
        return 2
    summary = write_summary(Path(argv[0]))
    print(Path(argv[0]) / "acceptance_summary.md")
    return 0 if summary.get("verdict") == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
