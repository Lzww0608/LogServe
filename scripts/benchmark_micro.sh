#!/usr/bin/env bash
set -euo pipefail

# Runs the repository microbenchmark suite and writes both raw Go benchmark
# output and parsed JSON metrics. Environment variables select the output id,
# benchmark duration, and optional Python interpreter without changing CI wiring.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
cd "$ROOT"

# Output names are intentionally routed through naming_guard so benchmark
# artifacts stay canonical instead of accumulating timestamped result names.
OUT_DIR="${LOGSERVE_MICRO_BENCH_OUT:-$ROOT/benchmarks}"
mkdir -p "$OUT_DIR"
BENCH_ID="${LOGSERVE_MICRO_BENCH_ID:-latest}"
logserve_reject_dated_name "$BENCH_ID" "LOGSERVE_MICRO_BENCH_ID"
OUT_FILE="$OUT_DIR/micro-${BENCH_ID}.txt"
JSON_FILE="$OUT_DIR/micro-${BENCH_ID}.json"
logserve_reject_dated_name "$OUT_FILE" "microbenchmark text output"
logserve_reject_dated_name "$JSON_FILE" "microbenchmark JSON output"

BENCHTIME="${LOGSERVE_GO_BENCHTIME:-300ms}"
GO_ENV=(env -u LOGSERVE_API_TOKEN -u LOGSERVE_SCHEDULER_V2)

# run_bench appends one Go benchmark package to the shared text output while
# clearing environment toggles that would make local benchmark numbers depend
# on a caller's authenticated console or scheduler settings.
run_bench() {
  local name="$1"
  shift
  echo "==> $name"
  "${GO_ENV[@]}" go test "$@" -run '^$' -benchmem -benchtime "$BENCHTIME" | tee -a "$OUT_FILE"
}

: >"$OUT_FILE"

# Keep each package invocation separate so a slow or allocation-heavy subsystem
# can be identified directly in the combined text output.
run_bench logstore_append ./internal/logstore -bench 'BenchmarkStoreAppend|BenchmarkStoreRecover'
run_bench logstore_read ./internal/logstore -bench 'BenchmarkRead|BenchmarkEncodeRecord'
run_bench control_scheduler ./internal/control -bench 'BenchmarkSchedulerAssignMixedBacklog|BenchmarkPreferred'
run_bench workflow_dag ./internal/workflow -bench 'BenchmarkSchedule'
run_bench metadata ./internal/metadata -bench 'BenchmarkMemoryStore' \
  -memprofile "$OUT_DIR/metadata_heap-${BENCH_ID}.pprof" \
  -mutexprofile "$OUT_DIR/metadata_mutex-${BENCH_ID}.pprof"
run_bench bootstrap ./internal/control -bench 'BenchmarkBootstrapFromLog'

PYTHON="${LOGSERVE_PYTHON:-python3}"
# Python benchmarks are optional: missing interpreters or unavailable runtime
# services should not prevent core Go microbenchmarks from producing reports.
if command -v "$PYTHON" >/dev/null 2>&1; then
  echo "==> python_executor_bench" | tee -a "$OUT_FILE"
  "$PYTHON" examples/evaluation/executor_bench.py | tee -a "$OUT_FILE" || echo "python_executor_bench failed" | tee -a "$OUT_FILE"
  if [ -n "${LOGSERVE_CONTROL_ADDR:-}" ] && PYTHONPATH="$ROOT/sdk/python${PYTHONPATH:+:$PYTHONPATH}" "$PYTHON" -c "import logserve" >/dev/null 2>&1; then
    echo "==> checkpoint_cache_bench" | tee -a "$OUT_FILE"
    PYTHONPATH="$ROOT/sdk/python${PYTHONPATH:+:$PYTHONPATH}" "$PYTHON" examples/evaluation/checkpoint_cache_bench.py | tee -a "$OUT_FILE" || echo "checkpoint_cache_bench failed" | tee -a "$OUT_FILE"
  else
    echo "==> checkpoint_cache_bench skipped (set LOGSERVE_CONTROL_ADDR and sdk/python on PYTHONPATH)" | tee -a "$OUT_FILE"
  fi
fi

"$PYTHON" scripts/parse_go_benchmark.py "$OUT_FILE" >"$JSON_FILE"
# The JSON file contains only Go benchmark rows; Python helper output remains
# in the raw text report for manual inspection.

echo
echo "Microbenchmark text: $OUT_FILE"
echo "Microbenchmark json: $JSON_FILE"
