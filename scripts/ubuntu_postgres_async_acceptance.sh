#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${LOGSERVE_UBUNTU_ACCEPTANCE_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"
RESULT_DIR="${LOGSERVE_UBUNTU_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-postgres-async-$RUN_ID"}"
STATUS_FILE="$RESULT_DIR/command_status.jsonl"
COMPARE_DIR="$RESULT_DIR/postgres_async_compare"
PACKAGE_PATH="$RESULT_DIR/ubuntu-acceptance-package.tar.gz"
PYTHON_BOOTSTRAP="${PYTHON:-python3}"
PYTHON_RUN="$PYTHON_BOOTSTRAP"
ANY_FAIL=0
PREREQ_OK=1

mkdir -p "$RESULT_DIR"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

json_escape() {
  "$PYTHON_BOOTSTRAP" - "$1" <<'PY'
import json
import sys
print(json.dumps(sys.argv[1])[1:-1])
PY
}

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

detect_compose() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

check_prerequisites() {
  local missing=0
  for cmd in bash git go tar "$PYTHON_BOOTSTRAP"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "missing required command: $cmd"
      missing=1
    fi
  done
  if ! command -v docker >/dev/null 2>&1; then
    echo "missing required command: docker"
    missing=1
  elif ! docker info >/dev/null 2>&1; then
    echo "docker is installed but the daemon is not reachable"
    missing=1
  fi
  if ! detect_compose; then
    echo "missing Docker Compose: install docker compose plugin or docker-compose"
    missing=1
  fi
  return "$missing"
}

write_server_environment() {
  {
    echo "run_id=$RUN_ID"
    echo "root=$ROOT"
    echo "result_dir=$RESULT_DIR"
    echo "compare_dir=$COMPARE_DIR"
    echo "generated_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    uname -a || true
    if command -v lsb_release >/dev/null 2>&1; then lsb_release -a || true; fi
    if command -v nproc >/dev/null 2>&1; then echo "cpus=$(nproc)"; fi
    if [ -r /proc/meminfo ]; then grep -E '^(MemTotal|MemAvailable):' /proc/meminfo || true; fi
    echo
    go version || true
    "$PYTHON_BOOTSTRAP" --version || true
    docker --version 2>/dev/null || true
    docker compose version 2>/dev/null || docker-compose --version 2>/dev/null || true
    git rev-parse --short HEAD 2>/dev/null || true
    git status --short 2>/dev/null || true
  } > "$RESULT_DIR/server_environment.txt"
}

write_acceptance_summary() {
  "$PYTHON_BOOTSTRAP" - "$RESULT_DIR" "$COMPARE_DIR" "$PACKAGE_PATH" <<'PY'
import json
import sys
from pathlib import Path

result_dir = Path(sys.argv[1])
compare_dir = Path(sys.argv[2])
package_path = Path(sys.argv[3])
statuses = []
status_path = result_dir / "command_status.jsonl"
if status_path.exists():
    for line in status_path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        try:
            statuses.append(json.loads(line))
        except json.JSONDecodeError:
            statuses.append({"name": "malformed_status_line", "exit_code": 1, "duration_sec": 0, "log": ""})

comparison = {}
comparison_path = compare_dir / "comparison.json"
if comparison_path.exists():
    try:
        comparison = json.loads(comparison_path.read_text(encoding="utf-8-sig"))
    except json.JSONDecodeError:
        comparison = {}

failed = [item for item in statuses if int(item.get("exit_code", 1)) != 0]
comparison_acceptance = ((comparison.get("acceptance") or {}).get("pass") is True)
verdict = "pass" if not failed and comparison_acceptance else "fail"
summary = {
    "verdict": verdict,
    "result_dir": str(result_dir),
    "package": str(package_path),
    "compare_dir": str(compare_dir),
    "failed_commands": [item.get("name") for item in failed],
    "commands": statuses,
    "comparison": comparison,
    "send_back": [
        str(result_dir / "acceptance_summary.md"),
        str(result_dir / "acceptance_summary.json"),
        str(compare_dir / "summary.md"),
        str(compare_dir / "comparison.json"),
        str(package_path),
    ],
}
(result_dir / "acceptance_summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")

lines = ["# Ubuntu PostgreSQL Async Acceptance Summary", ""]
lines.append(f"- Verdict: **{verdict.upper()}**")
lines.append(f"- Result directory: `{result_dir}`")
lines.append(f"- Package: `{package_path}`")
lines.append(f"- Compare summary: `{compare_dir / 'summary.md'}`")
lines.append("")
lines.append("## Commands")
lines.append("")
lines.append("| Command | Status | Seconds | Log |")
lines.append("|---|---:|---:|---|")
for item in statuses:
    status = "PASS" if int(item.get("exit_code", 1)) == 0 else "FAIL"
    lines.append(f"| `{item.get('name')}` | {status} | {item.get('duration_sec', 0)} | `{item.get('log', '')}` |")
if comparison:
    lines.append("")
    lines.append("## PostgreSQL Async Comparison")
    lines.append("")
    lines.append(f"- Acceptance: `{'PASS' if comparison_acceptance else 'FAIL'}`")
    for key, passed in ((comparison.get("acceptance") or {}).get("checks") or {}).items():
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

setup_python() {
  if [ "${LOGSERVE_USE_VENV:-1}" != "1" ]; then
    PYTHON_RUN="$PYTHON_BOOTSTRAP"
    return 0
  fi
  run_step python_venv_create "$RESULT_DIR/python_venv_create.log" "$PYTHON_BOOTSTRAP" -m venv "$RESULT_DIR/venv"
  PYTHON_RUN="$RESULT_DIR/venv/bin/python"
  if [ ! -x "$PYTHON_RUN" ]; then
    PYTHON_RUN="$PYTHON_BOOTSTRAP"
    return 1
  fi
  run_step python_pip_install "$RESULT_DIR/python_pip_install.log" "$PYTHON_RUN" -m pip install -r sdk/python/requirements.txt
  return 0
}
print_failure_context() {
  if [ "$ANY_FAIL" -eq 0 ]; then
    return 0
  fi
  echo
  echo "==> failure_summary"
  if [ -f "$RESULT_DIR/acceptance_summary.md" ]; then
    cat "$RESULT_DIR/acceptance_summary.md"
  fi
  if [ -f "$COMPARE_DIR/summary.md" ]; then
    echo
    cat "$COMPARE_DIR/summary.md"
  elif [ -f "$RESULT_DIR/postgres_async_compare.log" ]; then
    echo
    tail -120 "$RESULT_DIR/postgres_async_compare.log" || true
  fi
}
package_results() {
  local tmp_package="$RESULT_DIR/../.$(basename "$RESULT_DIR").tmp.tar.gz"
  rm -f "$tmp_package" "$PACKAGE_PATH"
  tar \
    --exclude '*/runtime' \
    --exclude '*/venv' \
    --exclude './ubuntu-acceptance-package.tar.gz' \
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

write_server_environment
run_step prerequisite_check "$RESULT_DIR/prerequisite_check.log" check_prerequisites
if [ "$ANY_FAIL" -ne 0 ]; then
  PREREQ_OK=0
fi
if [ "$PREREQ_OK" -eq 1 ]; then
  if ! setup_python; then
    PREREQ_OK=0
  fi
  if [ "$ANY_FAIL" -ne 0 ]; then
    PREREQ_OK=0
  fi
fi

if [ "${LOGSERVE_SERVER_SKIP_BASELINE:-0}" != "1" ]; then
  run_step go_test_all "$RESULT_DIR/go_test_all.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_SCHEDULER_V2 go test -count=1 ./...
  run_step go_race_metadata_control "$RESULT_DIR/go_race_metadata_control.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_SCHEDULER_V2 go test -race -count=1 ./internal/metadata ./internal/control
  run_step python_unittest "$RESULT_DIR/python_unittest.log" "$PYTHON_RUN" -m unittest discover sdk/python/tests
  run_step python_compileall "$RESULT_DIR/python_compileall.log" "$PYTHON_RUN" -m compileall -q sdk/python/logserve scripts
fi

if [ "$PREREQ_OK" -eq 1 ]; then
  run_step postgres_async_compare "$RESULT_DIR/postgres_async_compare.log" env \
    LOGSERVE_POSTGRES_COMPARE_DIR="$COMPARE_DIR" \
    LOGSERVE_COMPARE_RUN_FULL_TESTS="${LOGSERVE_COMPARE_RUN_FULL_TESTS:-0}" \
    LOGSERVE_COMPARE_RUN_RACE="${LOGSERVE_COMPARE_RUN_RACE:-0}" \
    LOGSERVE_COMPARE_RUN_GO_BENCH="${LOGSERVE_COMPARE_RUN_GO_BENCH:-0}" \
    LOGSERVE_COMPARE_RUN_LOGSTORE_BENCH="${LOGSERVE_COMPARE_RUN_LOGSTORE_BENCH:-0}" \
    LOGSERVE_COMPARE_RUN_FAULT="${LOGSERVE_COMPARE_RUN_FAULT:-0}" \
    LOGSERVE_COMPARE_REQUIRE_IMPROVEMENT="${LOGSERVE_COMPARE_REQUIRE_IMPROVEMENT:-1}" \
    LOGSERVE_COMPARE_BENCH_TASKS="${LOGSERVE_COMPARE_BENCH_TASKS:-64}" \
    LOGSERVE_COMPARE_BENCH_WORKFLOWS="${LOGSERVE_COMPARE_BENCH_WORKFLOWS:-5}" \
    LOGSERVE_COMPARE_BENCH_LLM_REQUESTS="${LOGSERVE_COMPARE_BENCH_LLM_REQUESTS:-10}" \
    LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS="${LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS:-40}" \
    PYTHON="$PYTHON_RUN" \
    bash scripts/postgres_async_compare.sh
else
  echo "Skipping postgres_async_compare because prerequisite_check failed" > "$RESULT_DIR/postgres_async_compare.log"
  record_status postgres_async_compare 1 0 postgres_async_compare.log
fi

write_acceptance_summary
package_results
write_acceptance_summary >> "$RESULT_DIR/acceptance_summary.log" 2>&1 || true
print_failure_context

echo
echo "Ubuntu acceptance directory: $RESULT_DIR"
echo "Summary: $RESULT_DIR/acceptance_summary.md"
echo "Package: $PACKAGE_PATH"
echo "PostgreSQL comparison: $COMPARE_DIR/summary.md"
exit "$ANY_FAIL"
