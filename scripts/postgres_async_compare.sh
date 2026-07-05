#!/usr/bin/env bash
set -uo pipefail

# Runs the experiment harness twice, once with sync PostgreSQL persistence and
# once with async persistence, then summarizes the comparison. The script keeps
# going after a mode failure so the final summary can explain partial results.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
# Comparison output names are guarded to keep report directories stable and to
# avoid date-stamped paths that are hard to reference from project docs.
RUN_ID="${LOGSERVE_POSTGRES_COMPARE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_POSTGRES_COMPARE_ID"
COMPARE_DIR="${LOGSERVE_POSTGRES_COMPARE_DIR:-"$ROOT/reports/postgres-async-compare-$RUN_ID"}"
logserve_reject_dated_name "$COMPARE_DIR" "LOGSERVE_POSTGRES_COMPARE_DIR"
TASKS="${LOGSERVE_COMPARE_BENCH_TASKS:-32}"
WORKFLOWS="${LOGSERVE_COMPARE_BENCH_WORKFLOWS:-3}"
LLM_REQUESTS="${LOGSERVE_COMPARE_BENCH_LLM_REQUESTS:-6}"
ACTOR_COMMANDS="${LOGSERVE_COMPARE_BENCH_ACTOR_COMMANDS:-20}"
PYTHON_BOOTSTRAP="${PYTHON:-python3}"
# ANY_FAIL accumulates mode and summary failures; run_mode returns success so
# both sync and async modes are attempted before the wrapper exits.
ANY_FAIL=0

mkdir -p "$COMPARE_DIR"
cd "$ROOT" || exit 1

# run_mode invokes run_experiment.sh with only the benchmark suite enabled,
# writing each persistence mode into its own subdirectory for comparison.
run_mode() {
  local mode="$1"
  local dir="$COMPARE_DIR/$mode"
  echo "==> compose benchmark mode=$mode dir=$dir"
  env \
    LOGSERVE_EXPERIMENT_DIR="$dir" \
    LOGSERVE_POSTGRES_MODE="$mode" \
    LOGSERVE_RUN_FULL_TESTS="${LOGSERVE_COMPARE_RUN_FULL_TESTS:-0}" \
    LOGSERVE_RUN_RACE="${LOGSERVE_COMPARE_RUN_RACE:-0}" \
    LOGSERVE_RUN_GO_BENCH="${LOGSERVE_COMPARE_RUN_GO_BENCH:-0}" \
    LOGSERVE_RUN_LOGSTORE_BENCH="${LOGSERVE_COMPARE_RUN_LOGSTORE_BENCH:-0}" \
    LOGSERVE_RUN_FAULT="${LOGSERVE_COMPARE_RUN_FAULT:-0}" \
    LOGSERVE_RUN_BENCHMARK=1 \
    LOGSERVE_BENCH_TASKS="$TASKS" \
    LOGSERVE_BENCH_WORKFLOWS="$WORKFLOWS" \
    LOGSERVE_BENCH_LLM_REQUESTS="$LLM_REQUESTS" \
    LOGSERVE_BENCH_ACTOR_COMMANDS="$ACTOR_COMMANDS" \
    bash scripts/run_experiment.sh
  local code=$?
  if [ "$code" -ne 0 ]; then
    ANY_FAIL=1
  fi
  return 0
}

run_mode sync
run_mode async

# The summarizer performs the pass/fail comparison after both directories have
# been attempted, even if one experiment mode failed earlier.
"$PYTHON_BOOTSTRAP" scripts/summarize_postgres_async_compare.py "$COMPARE_DIR"
COMPARE_CODE=$?
if [ "$COMPARE_CODE" -ne 0 ]; then
  ANY_FAIL=1
fi

exit "$ANY_FAIL"
