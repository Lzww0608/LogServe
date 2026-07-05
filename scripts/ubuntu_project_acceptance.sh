#!/usr/bin/env bash
set -uo pipefail

# Runs the top-level Ubuntu project acceptance flow and packages all evidence.
# This wrapper records baseline gates and optional sub-suite results in one
# command_status.jsonl stream for the final project acceptance summary.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_UBUNTU_PROJECT_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_UBUNTU_PROJECT_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_UBUNTU_PROJECT_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-project-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_UBUNTU_PROJECT_ACCEPTANCE_DIR"
STATUS_FILE="$RESULT_DIR/command_status.jsonl"
PACKAGE_PATH="$RESULT_DIR/ubuntu-project-acceptance-package.tar.gz"
COMPOSE_DIR="$RESULT_DIR/compose_experiment"
CHECKPOINT_DIR="$RESULT_DIR/checkpoint_acceptance"
POSTGRES_DIR="$RESULT_DIR/postgres_async_acceptance"
PYTHON_BOOTSTRAP="${PYTHON:-python3}"
PYTHON_RUN="$PYTHON_BOOTSTRAP"
ANY_FAIL=0
PREREQ_OK=1
RUN_COMPOSE_EXPERIMENT="${LOGSERVE_PROJECT_RUN_COMPOSE:-1}"
RUN_CHECKPOINT_ACCEPTANCE="${LOGSERVE_PROJECT_RUN_CHECKPOINT:-1}"
RUN_POSTGRES_ASYNC_ACCEPTANCE="${LOGSERVE_PROJECT_RUN_POSTGRES_ASYNC:-1}"
# Keep the compaction race gates focused on destructive logstore paths so the
# expensive race run still validates the highest-risk physical compaction cases.
COMPACTION_TEST_REGEX='Test(CompactabilityStatsReportsSegmentLiveBytes|SegmentLevelCompactionDeletesFullyTrimmedSegment|CompactionDeleteWithoutManifestRecoversTrimmedNextSeq|CompactionManifestBeforeDeleteCrashCompletesDelete|CompactionManifestAfterDeleteCrashRecovers|CopyCompactionRewritesPartialLiveSegment|BackgroundCompactorDeletesFullyTrimmedSegment)$'

mkdir -p "$RESULT_DIR"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

# json_escape emits one shell argument as a JSON string fragment for status JSONL.
json_escape() {
  "$PYTHON_BOOTSTRAP" - "$1" <<'PY'
import json
import sys
print(json.dumps(sys.argv[1])[1:-1])
PY
}

# record_status appends one command result and updates ANY_FAIL without aborting later packaging steps.
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

# run_step executes a command into a log file, records duration and exit code, and returns success so subsequent evidence can still be collected.
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

# bool_enabled centralizes string flag parsing for LOGSERVE_PROJECT_* suite toggles.
bool_enabled() {
  case "${1:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

# detect_compose accepts either Docker Compose v2 or the legacy docker-compose binary.
detect_compose() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

# docker_required is true only when an enabled sub-suite needs Compose-backed infrastructure.
docker_required() {
  bool_enabled "$RUN_COMPOSE_EXPERIMENT" || bool_enabled "$RUN_POSTGRES_ASYNC_ACCEPTANCE"
}

# check_prerequisites requires Docker only for enabled Compose-dependent sub-suites; checkpoint-only runs can proceed without it.
check_prerequisites() {
  local missing=0
  for cmd in bash git go tar "$PYTHON_BOOTSTRAP"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "missing required command: $cmd"
      missing=1
    fi
  done
  if docker_required; then
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
  else
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 && detect_compose; then
      echo "docker compose: available"
    else
      echo "docker compose: unavailable; compose-dependent suites are disabled by run config"
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
    echo "compose_dir=$COMPOSE_DIR"
    echo "checkpoint_dir=$CHECKPOINT_DIR"
    echo "postgres_dir=$POSTGRES_DIR"
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

# write_run_config persists enabled sub-suite switches so the project summarizer can distinguish skipped work from failures.
write_run_config() {
  "$PYTHON_BOOTSTRAP" - "$RESULT_DIR/run_config.json" "$RUN_COMPOSE_EXPERIMENT" "$RUN_CHECKPOINT_ACCEPTANCE" "$RUN_POSTGRES_ASYNC_ACCEPTANCE" <<'PY'
import json
import sys
from pathlib import Path

# enabled keeps the JSON run config in sync with the shell-side suite flag parser.
def enabled(value):
    return str(value).lower() in {"1", "true", "yes", "on"}

path = Path(sys.argv[1])
config = {
    "run_compose_experiment": enabled(sys.argv[2]),
    "run_checkpoint_acceptance": enabled(sys.argv[3]),
    "run_postgres_async_acceptance": enabled(sys.argv[4]),
}
path.write_text(json.dumps(config, indent=2), encoding="utf-8")
PY
}

# setup_python optionally creates a result-local virtualenv so SDK dependencies do not leak into the host interpreter.
setup_python() {
  if [ "${LOGSERVE_USE_VENV:-1}" != "1" ]; then
    # CI images with preinstalled Python dependencies can opt out of the result-local virtualenv.
    PYTHON_RUN="$PYTHON_BOOTSTRAP"
    return 0
  fi
  run_step python_venv_create "$RESULT_DIR/python_venv_create.log" "$PYTHON_BOOTSTRAP" -m venv "$RESULT_DIR/venv"
  PYTHON_RUN="$RESULT_DIR/venv/bin/python"
  if [ ! -x "$PYTHON_RUN" ]; then
    # run_step records venv creation failures without aborting; this explicit check makes setup fail fast for Python-dependent gates.
    PYTHON_RUN="$PYTHON_BOOTSTRAP"
    return 1
  fi
  run_step python_pip_install "$RESULT_DIR/python_pip_install.log" "$PYTHON_RUN" -m pip install -r sdk/python/requirements.txt
  return 0
}

# write_acceptance_summary delegates project verdict construction to the Python summary script.
write_acceptance_summary() {
  "$PYTHON_RUN" scripts/summarize_ubuntu_project_acceptance.py "$RESULT_DIR" > "$RESULT_DIR/acceptance_summary.log" 2>&1 || true
}

# package_results archives the result directory after excluding heavy runtime directories, virtualenvs, and the package being written.
package_results() {
  local tmp_package="$RESULT_DIR/../.$(basename "$RESULT_DIR").tmp.tar.gz"
  # Write the archive outside RESULT_DIR first so tar never reads the file it is actively creating.
  rm -f "$tmp_package" "$PACKAGE_PATH"
  tar \
    --exclude '*/runtime' \
    --exclude '*/venv' \
    --exclude './ubuntu-project-acceptance-package.tar.gz' \
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

# print_failure_context prints the generated project summary when any recorded step failed.
print_failure_context() {
  if [ "$ANY_FAIL" -eq 0 ]; then
    return 0
  fi
  echo
  echo "==> failure_summary"
  if [ -f "$RESULT_DIR/acceptance_summary.md" ]; then
    cat "$RESULT_DIR/acceptance_summary.md"
  fi
}

# skip_enabled_suites_after_prereq_failure records enabled sub-suites as failed evidence when prerequisites prevent running them.
skip_enabled_suites_after_prereq_failure() {
  # Compose experiment is optional and receives its own output directory for nested summary/package artifacts.
  if bool_enabled "$RUN_COMPOSE_EXPERIMENT"; then
    echo "Skipping compose_experiment because prerequisite_check failed" > "$RESULT_DIR/compose_experiment.log"
    record_status compose_experiment 1 0 compose_experiment.log
  fi
  # Nested checkpoint acceptance skips its own baseline because this project wrapper already ran the shared gates.
  if bool_enabled "$RUN_CHECKPOINT_ACCEPTANCE"; then
    echo "Skipping checkpoint_acceptance because prerequisite_check failed" > "$RESULT_DIR/checkpoint_acceptance.log"
    record_status checkpoint_acceptance 1 0 checkpoint_acceptance.log
  fi
  # Nested PostgreSQL async acceptance also skips its baseline to avoid duplicating project-level validation.
  if bool_enabled "$RUN_POSTGRES_ASYNC_ACCEPTANCE"; then
    echo "Skipping postgres_async_acceptance because prerequisite_check failed" > "$RESULT_DIR/postgres_async_acceptance.log"
    record_status postgres_async_acceptance 1 0 postgres_async_acceptance.log
  fi
}

# Capture environment and run_config before checks so prereq failures still produce reviewable metadata.
write_server_environment
write_run_config
run_step prerequisite_check "$RESULT_DIR/prerequisite_check.log" check_prerequisites
if [ "$ANY_FAIL" -ne 0 ]; then
  PREREQ_OK=0
fi
# Python setup is gated by prerequisites because baseline and sub-suite scripts share PYTHON_RUN.
if [ "$PREREQ_OK" -eq 1 ]; then
  if ! setup_python; then
    PREREQ_OK=0
  fi
  if [ "$ANY_FAIL" -ne 0 ]; then
    PREREQ_OK=0
  fi
fi

# Baseline gates run before optional sub-suites so broad project failures are visible even when a sub-suite later fails.
if [ "$PREREQ_OK" -eq 1 ]; then
  # Clear ambient API tokens for suites that assert unauthenticated defaults or create their own auth context.
  run_step go_test_all "$RESULT_DIR/go_test_all.log" env -u LOGSERVE_API_TOKEN go test -count=1 ./...
  run_step go_vet "$RESULT_DIR/go_vet.log" go vet ./...
  run_step go_test_physical_compaction "$RESULT_DIR/go_test_physical_compaction.log" go test -count=1 ./internal/logstore -run "$COMPACTION_TEST_REGEX"
  run_step go_race_logstore "$RESULT_DIR/go_race_logstore.log" go test -race -count=1 ./internal/logstore -run "$COMPACTION_TEST_REGEX"
  run_step go_race_core "$RESULT_DIR/go_race_core.log" env -u LOGSERVE_API_TOKEN go test -race -count=1 ./internal/control ./internal/metadata ./internal/worker
  run_step python_script_tests "$RESULT_DIR/python_script_tests.log" "$PYTHON_RUN" -m unittest discover tests/scripts
  run_step python_sdk_tests "$RESULT_DIR/python_sdk_tests.log" "$PYTHON_RUN" -m unittest discover sdk/python/tests
  run_step python_compileall "$RESULT_DIR/python_compileall.log" "$PYTHON_RUN" -m compileall -q sdk/python/logserve scripts

  if bool_enabled "$RUN_COMPOSE_EXPERIMENT"; then
    # The compose experiment has its own result tree and disables broad gates already covered by this wrapper.
    run_step compose_experiment "$RESULT_DIR/compose_experiment.log" env \
      PYTHON="$PYTHON_RUN" \
      LOGSERVE_EXPERIMENT_DIR="$COMPOSE_DIR" \
      LOGSERVE_RUN_FULL_TESTS=0 \
      LOGSERVE_RUN_RACE=0 \
      LOGSERVE_RUN_GO_BENCH="${LOGSERVE_RUN_GO_BENCH:-1}" \
      LOGSERVE_RUN_LOGSTORE_BENCH="${LOGSERVE_RUN_LOGSTORE_BENCH:-1}" \
      LOGSERVE_RUN_FAULT="${LOGSERVE_RUN_FAULT:-1}" \
      LOGSERVE_RUN_BENCHMARK="${LOGSERVE_RUN_BENCHMARK:-1}" \
      bash scripts/run_experiment.sh
  fi

  if bool_enabled "$RUN_CHECKPOINT_ACCEPTANCE"; then
    # Nested acceptance suites skip baseline gates so project-level evidence remains the single source for those checks.
    run_step checkpoint_acceptance "$RESULT_DIR/checkpoint_acceptance.log" env \
      PYTHON="$PYTHON_RUN" \
      LOGSERVE_UBUNTU_CHECKPOINT_ACCEPTANCE_DIR="$CHECKPOINT_DIR" \
      LOGSERVE_CHECKPOINT_SKIP_BASELINE=1 \
      bash scripts/ubuntu_checkpoint_acceptance.sh
  fi

  if bool_enabled "$RUN_POSTGRES_ASYNC_ACCEPTANCE"; then
    # PostgreSQL async acceptance shares the same baseline-skip contract as checkpoint acceptance.
    run_step postgres_async_acceptance "$RESULT_DIR/postgres_async_acceptance.log" env \
      PYTHON="$PYTHON_RUN" \
      LOGSERVE_UBUNTU_ACCEPTANCE_DIR="$POSTGRES_DIR" \
      LOGSERVE_SERVER_SKIP_BASELINE=1 \
      bash scripts/ubuntu_postgres_async_acceptance.sh
  fi
else
  skip_enabled_suites_after_prereq_failure
fi

# Write a pre-package summary, add package status, then rewrite so final summary includes packaging evidence.
write_acceptance_summary
package_results
write_acceptance_summary
print_failure_context

echo
echo "Ubuntu project acceptance directory: $RESULT_DIR"
echo "Summary: $RESULT_DIR/acceptance_summary.md"
echo "JSON: $RESULT_DIR/acceptance_summary.json"
echo "Package: $PACKAGE_PATH"
exit "$ANY_FAIL"