#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT_DIR="${LOGSERVE_MICRO_BENCH_OUT:-$ROOT/benchmarks}"
mkdir -p "$OUT_DIR"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_FILE="$OUT_DIR/micro-${STAMP}.txt"
JSON_FILE="$OUT_DIR/micro-${STAMP}.json"

BENCHTIME="${LOGSERVE_GO_BENCHTIME:-300ms}"
GO_ENV=(env -u LOGSERVE_API_TOKEN -u LOGSERVE_SCHEDULER_V2)

run_bench() {
  local name="$1"
  shift
  echo "==> $name"
  "${GO_ENV[@]}" go test "$@" -run '^$' -benchmem -benchtime "$BENCHTIME" | tee -a "$OUT_FILE"
}

: >"$OUT_FILE"

run_bench logstore_append ./internal/logstore -bench 'BenchmarkStoreAppend|BenchmarkStoreRecover'
run_bench logstore_read ./internal/logstore -bench 'BenchmarkRead|BenchmarkEncodeRecord'
run_bench control_scheduler ./internal/control -bench 'BenchmarkSchedulerAssignMixedBacklog|BenchmarkPreferred'
run_bench workflow_dag ./internal/workflow -bench 'BenchmarkSchedule'
run_bench metadata ./internal/metadata -bench 'BenchmarkMemoryStore' \
  -memprofile "$OUT_DIR/metadata_heap-${STAMP}.pprof" \
  -mutexprofile "$OUT_DIR/metadata_mutex-${STAMP}.pprof"
run_bench bootstrap ./internal/control -bench 'BenchmarkBootstrapFromLog'

PYTHON="${LOGSERVE_PYTHON:-python3}"
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

echo
echo "Microbenchmark text: $OUT_FILE"
echo "Microbenchmark json: $JSON_FILE"
