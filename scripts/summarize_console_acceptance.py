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


FEATURE_GROUPS_6_10 = {
    "feature_6_workflow_dag": {
        "title": "Workflow DAG and replay",
        "checks": (
            "submit_workflow_via_console_api",
            "workflow_detail_has_dag_dependencies",
            "workflow_replay_consistent",
        ),
    },
    "feature_7_llm_playground": {
        "title": "LLM playground and replay trace",
        "checks": (
            "register_model_via_console_api",
            "submit_llm_via_console_api",
            "llm_replay_trace_has_latency",
        ),
    },
    "feature_8_worker_cache_matrix": {
        "title": "Worker cache matrix",
        "checks": ("worker_cache_matrix_has_model",),
    },
    "feature_9_actor_console": {
        "title": "Actor create, call, and status",
        "checks": (
            "create_actor_via_console_api",
            "call_actor_via_console_api",
            "get_actor_status",
        ),
    },
    "feature_10_log_stream_explorer": {
        "title": "Log stream explorer",
        "checks": (
            "log_streams_list_system_functions",
            "log_stream_read_system_functions",
            "log_stream_read_workflow",
            "log_stream_read_actor",
        ),
    },
}


def build_feature_groups_6_10(run_docker, probe_checks):
    features = {}
    states = []
    probe_checks = probe_checks or {}
    for feature_id, spec in FEATURE_GROUPS_6_10.items():
        check_results = {name: probe_checks.get(name) is True for name in spec["checks"]}
        missing_checks = [name for name in spec["checks"] if name not in probe_checks]
        failed_checks = [name for name, passed in check_results.items() if not passed]
        if not run_docker:
            state = "INCOMPLETE"
        elif failed_checks:
            state = "FAIL"
        else:
            state = "PASS"
        states.append(state)
        features[feature_id] = {
            "title": spec["title"],
            "state": state,
            "checks": check_results,
            "missing_checks": missing_checks,
            "failed_checks": failed_checks,
        }
    if all(state == "PASS" for state in states):
        verdict = "PASS"
    elif all(state == "INCOMPLETE" for state in states):
        verdict = "INCOMPLETE"
    else:
        verdict = "FAIL"
    return {"verdict": verdict, "features": features}


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
    features_6_10 = build_feature_groups_6_10(bool_config(config, "run_docker", True), probe.get("checks") or {})
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
        "features_6_10": features_6_10,
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
    features = summary.get("features_6_10") or {}
    if features:
        lines.append("")
        lines.append("## Features 6-10")
        lines.append("")
        lines.append(f"- Verdict: **{features.get('verdict', 'UNKNOWN')}**")
        lines.append("")
        lines.append("| Feature | State | Checks |")
        lines.append("|---|---:|---|")
        for feature_id, feature in (features.get("features") or {}).items():
            failed = feature.get("failed_checks") or []
            checks_text = "all pass" if not failed else ", ".join(failed)
            lines.append(f"| `{feature_id}` {feature.get('title', '')} | {feature.get('state')} | {checks_text} |")
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
