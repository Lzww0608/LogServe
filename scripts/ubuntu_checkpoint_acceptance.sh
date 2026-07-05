#!/usr/bin/env bash
set -uo pipefail

# Runs the Ubuntu metadata-checkpoint acceptance suite and packages all evidence.
# The wrapper records every command in command_status.jsonl so later summaries can
# distinguish failed commands from missing or failed checkpoint checks.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_UBUNTU_CHECKPOINT_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_UBUNTU_CHECKPOINT_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_UBUNTU_CHECKPOINT_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-checkpoint-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_UBUNTU_CHECKPOINT_ACCEPTANCE_DIR"
STATUS_FILE="$RESULT_DIR/command_status.jsonl"
CHECKPOINT_DIR="$RESULT_DIR/checkpoint_acceptance"
PACKAGE_PATH="$RESULT_DIR/ubuntu-checkpoint-acceptance-package.tar.gz"
PYTHON_RUN="${PYTHON:-python3}"
ANY_FAIL=0
PREREQ_OK=1

mkdir -p "$RESULT_DIR"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

# json_escape emits one shell argument as a JSON string fragment for status JSONL.
json_escape() {
  "$PYTHON_RUN" - "$1" <<'PY'
import json
import sys
print(json.dumps(sys.argv[1])[1:-1])
PY
}

# record_status appends one command result and updates ANY_FAIL without stopping the script.
record_status() {
  local name="$1"
  local code="$2"
  local duration="$3"
  local log_name="$4"
  local escaped_name escaped_log
  escaped_name="$(json_escape "$name")"
  escaped_log="$(json_escape "$log_name")"
  printf '{"name":"%s","exit_code":%d,"duration_sec":%d,"log":"%s"}\n' \
    "$escaped_name" "$code" "$duration" "$escaped_log" >> "$STATUS_FILE"
  if [ "$code" -ne 0 ]; then
    ANY_FAIL=1
  fi
}

# run_step executes a command into a log file, records duration and exit code, and always returns success so later evidence can still be collected.
run_step() {
  local name="$1"
  local log="$2"
  shift 2
  local start end code
  echo "==> $name"
  start="$(date +%s)"
  "$@" > "$log" 2>&1
  code=$?
  end="$(date +%s)"
  record_status "$name" "$code" "$((end - start))" "$(basename "$log")"
  if [ "$code" -ne 0 ]; then
    echo "    failed: $log"
  else
    echo "    ok: $log"
  fi
  return 0
}

# detect_compose accepts either the Docker Compose v2 plugin or the legacy docker-compose binary.
detect_compose() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

# check_prerequisites verifies the host tools required for this wrapper and treats Docker as optional unless LOGSERVE_REQUIRE_DOCKER=1.
check_prerequisites() {
  local missing=0
  for cmd in bash git go tar "$PYTHON_RUN"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "missing required command: $cmd"
      missing=1
    fi
  done
  # Docker is only a prerequisite when explicitly requested; the nested
  # checkpoint harness can run in its in-memory single-host mode without it.
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 && detect_compose; then
    echo "docker compose: available"
  else
    echo "docker compose: unavailable; checkpoint acceptance will use the in-memory single-host harness"
    if [ "${LOGSERVE_REQUIRE_DOCKER:-0}" = "1" ]; then
      missing=1
    fi
  fi
  return "$missing"
}

# write_server_environment captures host, toolchain, Docker, git, and working-tree context for later review.
write_server_environment() {
  {
    echo "run_id=$RUN_ID"
    echo "root=$ROOT"
    echo "result_dir=$RESULT_DIR"
    echo "checkpoint_dir=$CHECKPOINT_DIR"
    echo "generated_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    uname -a || true
    if command -v lsb_release >/dev/null 2>&1; then lsb_release -a || true; fi
    if command -v nproc >/dev/null 2>&1; then echo "cpus=$(nproc)"; fi
    if [ -r /proc/meminfo ]; then grep -E '^(MemTotal|MemAvailable):' /proc/meminfo || true; fi
    echo
    go version || true
    "$PYTHON_RUN" --version || true
    docker --version 2>/dev/null || true
    docker compose version 2>/dev/null || docker-compose --version 2>/dev/null || true
    git rev-parse --short HEAD 2>/dev/null || true
    git status --short 2>/dev/null || true
  } > "$RESULT_DIR/server_environment.txt"
}

# write_acceptance_summary combines command_status.jsonl with the nested checkpoint summary and writes top-level handoff files.
write_acceptance_summary() {
  "$PYTHON_RUN" - "$RESULT_DIR" "$CHECKPOINT_DIR" "$PACKAGE_PATH" <<'PY'
import json
import sys
from pathlib import Path

result_dir = Path(sys.argv[1])
checkpoint_dir = Path(sys.argv[2])
package_path = Path(sys.argv[3])
statuses = []
status_path = result_dir / "command_status.jsonl"
if status_path.exists():
    for line in status_path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        try:
            statuses.append(json.loads(line))
        # A malformed status line means the evidence stream itself is damaged;
        # keep it as an explicit failed command instead of hiding it.
        except json.JSONDecodeError:
            statuses.append({"name": "malformed_status_line", "exit_code": 1, "duration_sec": 0, "log": ""})

checkpoint_summary = {}
summary_path = checkpoint_dir / "summary.json"
# The nested checkpoint summary is optional evidence; missing or malformed JSON
# becomes a failed top-level verdict rather than a Python exception.
if summary_path.exists():
    try:
        checkpoint_summary = json.loads(summary_path.read_text(encoding="utf-8-sig"))
    except json.JSONDecodeError:
        checkpoint_summary = {}

failed = [item for item in statuses if int(item.get("exit_code", 1)) != 0]
checkpoint_pass = checkpoint_summary.get("verdict") == "PASS"
# The wrapper is strict: shell command execution and the nested semantic
# checkpoint checks must both pass before the handoff package is marked PASS.
verdict = "PASS" if not failed and checkpoint_pass else "FAIL"
summary = {
    "verdict": verdict,
    "result_dir": str(result_dir),
    "package": str(package_path),
    "checkpoint_dir": str(checkpoint_dir),
    "failed_commands": [item.get("name") for item in failed],
    "commands": statuses,
    "checkpoint_acceptance": checkpoint_summary,
    # Include both top-level and nested artifacts in the handoff list so a
    # reviewer can inspect command status and checkpoint-specific evidence together.
    "send_back": [
        str(result_dir / "acceptance_summary.md"),
        str(result_dir / "acceptance_summary.json"),
        str(checkpoint_dir / "summary.md"),
        str(checkpoint_dir / "summary.json"),
        str(checkpoint_dir / "checkpoint_acceptance.json"),
        str(package_path),
    ],
}
(result_dir / "acceptance_summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")

lines = ["# Ubuntu Metadata Checkpoint Acceptance Summary", ""]
lines.append(f"- Verdict: **{verdict}**")
lines.append(f"- Result directory: `{result_dir}`")
lines.append(f"- Package: `{package_path}`")
lines.append(f"- Checkpoint summary: `{checkpoint_dir / 'summary.md'}`")
lines.append("")
lines.append("## Commands")
lines.append("")
lines.append("| Command | Status | Seconds | Log |")
lines.append("|---|---:|---:|---|")
for item in statuses:
    status = "PASS" if int(item.get("exit_code", 1)) == 0 else "FAIL"
    lines.append(f"| `{item.get('name')}` | {status} | {item.get('duration_sec', 0)} | `{item.get('log', '')}` |")
if checkpoint_summary:
    acceptance = checkpoint_summary.get("acceptance") or {}
    ratios = acceptance.get("ratios") or {}
    checks = acceptance.get("checks") or {}
    lines.append("")
    lines.append("## Checkpoint Acceptance")
    lines.append("")
    lines.append(f"- Verdict: `{checkpoint_summary.get('verdict')}`")
    lines.append(f"- Records read ratio: `{ratios.get('checkpoint_records_over_full')}`")
    lines.append(f"- Duration ratio: `{ratios.get('checkpoint_duration_over_full')}`")
    for key, passed in sorted(checks.items()):
        lines.append(f"- `{key}`: {'pass' if passed else 'fail'}")
lines.append("")
lines.append("## Send Back")
lines.append("")
for path in summary["send_back"]:
    lines.append(f"- `{path}`")
(result_dir / "acceptance_summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
print(result_dir / "acceptance_summary.md")
PY
}

# package_results archives the result directory after excluding transient virtualenvs and the package being written.
package_results() {
  local tmp_package="$RESULT_DIR/../.$(basename "$RESULT_DIR").tmp.tar.gz"
  # Write through a sibling temp archive so an interrupted tar run never leaves
  # a corrupt file at PACKAGE_PATH.
  rm -f "$tmp_package" "$PACKAGE_PATH"
  tar \
    --exclude '*/venv' \
    --exclude './ubuntu-checkpoint-acceptance-package.tar.gz' \
    -czf "$tmp_package" -C "$RESULT_DIR" . > "$RESULT_DIR/package.log" 2>&1
  local code=$?
  if [ "$code" -eq 0 ]; then
    mv "$tmp_package" "$PACKAGE_PATH" >> "$RESULT_DIR/package.log" 2>&1
    code=$?
  else
    rm -f "$tmp_package"
  fi
  record_status package_results "$code" 0 package.log
  return "$code"
}

# print_failure_context prints the most useful generated summary or tail log when any recorded step failed.
print_failure_context() {
  if [ "$ANY_FAIL" -eq 0 ]; then
    return 0
  fi
  echo
  echo "==> failure_summary"
  if [ -f "$RESULT_DIR/acceptance_summary.md" ]; then
    cat "$RESULT_DIR/acceptance_summary.md"
  fi
  if [ -f "$CHECKPOINT_DIR/summary.md" ]; then
    echo
    cat "$CHECKPOINT_DIR/summary.md"
  elif [ -f "$RESULT_DIR/checkpoint_acceptance.log" ]; then
    tail -120 "$RESULT_DIR/checkpoint_acceptance.log" || true
  fi
}

# Capture environment before running checks so prereq failures still leave host context.
write_server_environment
run_step prerequisite_check "$RESULT_DIR/prerequisite_check.log" check_prerequisites
if [ "$ANY_FAIL" -ne 0 ]; then
  PREREQ_OK=0
fi

# Baseline tests are optional so the checkpoint acceptance workload can be rerun quickly after prior validation.
if [ "$PREREQ_OK" -eq 1 ] && [ "${LOGSERVE_CHECKPOINT_SKIP_BASELINE:-0}" != "1" ]; then
  # Clear runtime-only env vars so baseline tests use repository defaults rather
  # than credentials or artifact paths from this wrapper.
  run_step go_test_all "$RESULT_DIR/go_test_all.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT go test -count=1 ./...
  run_step go_race_control_checkpoint "$RESULT_DIR/go_race_control_checkpoint.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT go test -race -count=1 ./internal/control \
    -run 'Test(Bootstrap.*Checkpoint|MetadataCheckpoint.*|CreateMetadataCheckpoint.*|ControlRestartBootstrapsTaskAfterMetadataWriteLoss)'
  run_step python_script_tests "$RESULT_DIR/python_script_tests.log" "$PYTHON_RUN" -m unittest discover tests/scripts
  run_step python_compileall "$RESULT_DIR/python_compileall.log" "$PYTHON_RUN" -m compileall -q scripts
fi

# The nested checkpoint runner receives all workload sizing through environment variables for reproducible reruns.
if [ "$PREREQ_OK" -eq 1 ]; then
  run_step checkpoint_acceptance "$RESULT_DIR/checkpoint_acceptance.log" env \
    PYTHON="$PYTHON_RUN" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_DIR="$CHECKPOINT_DIR" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_FULL_TESTS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_FULL_TESTS:-0}" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_RACE="${LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_RACE:-0}" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS:-120}" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS:-12}" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS:-12}" \
    LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS:-40}" \
    bash scripts/checkpoint_acceptance.sh
else
  echo "Skipping checkpoint_acceptance because prerequisite_check failed" > "$RESULT_DIR/checkpoint_acceptance.log"
  # Record the skipped nested run as a failed command so the top-level summary
  # explains why checkpoint evidence is missing.
  record_status checkpoint_acceptance 1 0 checkpoint_acceptance.log
fi

# Write a pre-package summary, add package_results to command_status.jsonl, then
# rewrite the summary so the final report includes packaging evidence.
write_acceptance_summary
package_results
write_acceptance_summary >> "$RESULT_DIR/acceptance_summary.log" 2>&1 || true
print_failure_context

echo
echo "Ubuntu checkpoint acceptance directory: $RESULT_DIR"
echo "Summary: $RESULT_DIR/acceptance_summary.md"
echo "Package: $PACKAGE_PATH"
echo "Checkpoint acceptance: $CHECKPOINT_DIR/summary.md"
exit "$ANY_FAIL"
