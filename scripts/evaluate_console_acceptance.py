#!/usr/bin/env python3
import argparse
import json
import shutil
import sys
import tarfile
import tempfile
from pathlib import Path


REQUIRED_LOCAL_CHECKS = (
    "go_web_tests",
    "go_web_vet",
    "web_build",
    "python_script_tests",
)

REQUIRED_DOCKER_CHECKS = (
    "docker_compose_config",
    "docker_compose_build",
    "docker_compose_up",
    "web_health_ready",
    "console_http_probe",
)

REQUIRED_PROBE_CHECKS = (
    "healthz_without_auth",
    "dashboard_requires_auth",
    "dashboard_with_auth",
    "static_root",
    "static_deep_link",
    "submit_task_via_console_api",
    "get_task_detail",
    "task_visible_in_dashboard_view",
    "submit_workflow_via_console_api",
    "workflow_detail_has_dag_dependencies",
    "workflow_replay_consistent",
    "register_model_via_console_api",
    "submit_llm_via_console_api",
    "llm_replay_trace_has_latency",
    "worker_cache_matrix_has_model",
    "create_actor_via_console_api",
    "call_actor_via_console_api",
    "get_actor_status",
    "log_streams_list_system_functions",
    "log_stream_read_system_functions",
    "log_stream_read_workflow",
    "log_stream_read_actor",
    "admin_config_with_auth",
)


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8-sig"))


def safe_extract_tar(package_path, dest):
    with tarfile.open(package_path, "r:gz") as archive:
        dest = dest.resolve()
        for member in archive.getmembers():
            target = (dest / member.name).resolve()
            if target != dest and dest not in target.parents:
                raise ValueError(f"unsafe tar member path: {member.name}")
            if member.issym() or member.islnk():
                raise ValueError(f"unsafe tar link member: {member.name}")
            if member.isdev():
                raise ValueError(f"unsafe tar device member: {member.name}")
        archive.extractall(dest)


def materialize_input(path):
    path = Path(path)
    if path.is_dir():
        return path, None
    if path.is_file() and path.suffixes[-2:] == [".tar", ".gz"]:
        tmp = Path(tempfile.mkdtemp(prefix="logserve-console-acceptance-"))
        try:
            safe_extract_tar(path, tmp)
        except Exception:
            shutil.rmtree(tmp, ignore_errors=True)
            raise
        return tmp, tmp
    raise ValueError(f"expected result directory or .tar.gz package: {path}")


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


def missing_or_failed(checks, names):
    return [name for name in names if checks.get(name) is not True]


def failed_commands(summary):
    return [name for name in summary.get("failed_commands") or [] if name]


def failed_checks(summary):
    return [name for name in summary.get("failed_checks") or [] if name]


def evaluate_result(root):
    root = Path(root)
    summary_path = root / "acceptance_summary.json"
    if not summary_path.exists():
        return {
            "verdict": "FAIL",
            "reason": "missing acceptance_summary.json",
            "result_dir": str(root),
            "failures": ["missing acceptance_summary.json"],
            "features_6_10": build_feature_groups_6_10(False, {}),
            "summary": {},
        }

    summary = read_json(summary_path)
    checks = summary.get("checks") or {}
    probe = summary.get("probe") or {}
    probe_checks = probe.get("checks") or {}
    run_config = summary.get("run_config") or {}
    failures = []
    warnings = []

    if summary.get("verdict") != "PASS":
        failures.append(f"summary verdict is {summary.get('verdict')!r}, expected 'PASS'")
    for name in failed_commands(summary):
        failures.append(f"failed command: {name}")
    for name in failed_checks(summary):
        failures.append(f"failed check: {name}")
    for name in missing_or_failed(checks, REQUIRED_LOCAL_CHECKS):
        failures.append(f"required local check failed or missing: {name}")

    if bool_config(run_config, "run_npm_ci", True) and checks.get("web_npm_ci") is not True:
        failures.append("required npm install check failed or missing: web_npm_ci")

    run_docker = bool_config(run_config, "run_docker", False)
    features_6_10 = build_feature_groups_6_10(run_docker, probe_checks)
    if not run_docker:
        if failures:
            return {
                "verdict": "FAIL",
                "reason": "console acceptance did not match expectations",
                "result_dir": str(root),
                "failures": failures,
                "warnings": warnings,
                "features_6_10": features_6_10,
                "summary": summary,
            }
        return {
            "verdict": "INCOMPLETE",
            "reason": "Docker console runtime was not exercised",
            "result_dir": str(root),
            "failures": failures,
            "warnings": warnings,
            "features_6_10": features_6_10,
            "summary": summary,
        }

    for name in missing_or_failed(checks, REQUIRED_DOCKER_CHECKS):
        failures.append(f"required Docker check failed or missing: {name}")

    if probe.get("verdict") != "PASS":
        failures.append(f"HTTP probe verdict is {probe.get('verdict')!r}, expected 'PASS'")
    for name in missing_or_failed(probe_checks, REQUIRED_PROBE_CHECKS):
        failures.append(f"required HTTP probe check failed or missing: {name}")
    for feature_id, feature in (features_6_10.get("features") or {}).items():
        if feature.get("state") != "PASS":
            failed = ", ".join(feature.get("failed_checks") or feature.get("missing_checks") or [])
            failures.append(f"feature 6-10 group failed: {feature_id} ({feature.get('state')}) {failed}".strip())

    return {
        "verdict": "FAIL" if failures else "PASS",
        "reason": "console acceptance matches expectations" if not failures else "console acceptance did not match expectations",
        "result_dir": str(root),
        "failures": failures,
        "warnings": warnings,
        "features_6_10": features_6_10,
        "summary": summary,
    }


def write_markdown(result, path):
    lines = ["# Console Acceptance Evaluation", ""]
    lines.append(f"- Verdict: **{result.get('verdict')}**")
    lines.append(f"- Reason: {result.get('reason')}")
    lines.append(f"- Result directory: `{result.get('result_dir')}`")
    failures = result.get("failures") or []
    warnings = result.get("warnings") or []
    if failures:
        lines.append("")
        lines.append("## Failures")
        lines.append("")
        for item in failures:
            lines.append(f"- {item}")
    if warnings:
        lines.append("")
        lines.append("## Warnings")
        lines.append("")
        for item in warnings:
            lines.append(f"- {item}")
    features = result.get("features_6_10") or {}
    if features:
        lines.append("")
        lines.append("## Features 6-10")
        lines.append("")
        lines.append(f"- Verdict: **{features.get('verdict', 'UNKNOWN')}**")
        lines.append("")
        lines.append("| Feature | State | Failed checks |")
        lines.append("|---|---:|---|")
        for feature_id, feature in (features.get("features") or {}).items():
            failed = feature.get("failed_checks") or []
            failed_text = "-" if not failed else ", ".join(failed)
            lines.append(f"| `{feature_id}` {feature.get('title', '')} | {feature.get('state')} | {failed_text} |")
    summary = result.get("summary") or {}
    if summary.get("checks"):
        lines.append("")
        lines.append("## Checks")
        lines.append("")
        for name, passed in sorted(summary.get("checks", {}).items()):
            lines.append(f"- `{name}`: {'pass' if passed else 'fail'}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main(argv=None):
    parser = argparse.ArgumentParser(description="Evaluate a LogServe Console acceptance result directory or package.")
    parser.add_argument("path", help="acceptance result directory or console-acceptance-package.tar.gz")
    parser.add_argument("--json-out", help="optional path for evaluation JSON")
    parser.add_argument("--md-out", help="optional path for evaluation Markdown")
    args = parser.parse_args(argv)

    temp_dir = None
    try:
        root, temp_dir = materialize_input(args.path)
        result = evaluate_result(root)
        if args.json_out:
            Path(args.json_out).write_text(json.dumps(result, indent=2, ensure_ascii=False), encoding="utf-8")
        if args.md_out:
            write_markdown(result, Path(args.md_out))
        print(json.dumps({k: result[k] for k in ("verdict", "reason", "failures", "warnings") if k in result}, indent=2, ensure_ascii=False))
        return 0 if result.get("verdict") == "PASS" else 1
    finally:
        if temp_dir is not None:
            shutil.rmtree(temp_dir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
