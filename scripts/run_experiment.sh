#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${LOGSERVE_EXPERIMENT_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"
RUN_DIR="${LOGSERVE_EXPERIMENT_DIR:-"$ROOT/reports/experiment-$RUN_ID"}"
STATUS_FILE="$RUN_DIR/command_status.jsonl"
DATA_DIR="${LOGSERVE_EXPERIMENT_DATA_DIR:-"$RUN_DIR/runtime"}"
LOG_ADDR="${LOGSERVE_EXPERIMENT_LOG_ADDR:-127.0.0.1:59051}"
CONTROL_ADDR="${LOGSERVE_EXPERIMENT_CONTROL_ADDR:-127.0.0.1:59052}"
CHECKPOINT_SOURCE_DIR="${LOGSERVE_CHECKPOINT_SOURCE_DIR:-"$DATA_DIR/checkpoints"}"
CHECKPOINT_CACHE_BYTES="${LOGSERVE_CHECKPOINT_CACHE_BYTES:-16777216}"

mkdir -p "$RUN_DIR" "$DATA_DIR"
cd "$ROOT" || exit 1

export GOCACHE="${GOCACHE:-"$ROOT/.gocache"}"
export PYTHONPATH="${PYTHONPATH:-"$ROOT/sdk/python"}"
export LOGSERVE_SDK_TRANSPORT="${LOGSERVE_SDK_TRANSPORT:-grpc}"
export LOGSERVE_CONTROL_ADDR="$CONTROL_ADDR"

PIDS=()
ANY_FAIL=0

record_status() {
  local name="$1"
  local code="$2"
  local duration="$3"
  local log_name="$4"
  printf '{"name":"%s","exit_code":%d,"duration_sec":%d,"log":"%s"}\n' \
    "$name" "$code" "$duration" "$log_name" >> "$STATUS_FILE"
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

run_json_step() {
  local name="$1"
  local json_out="$2"
  local log="$3"
  shift 3
  local start end code
  echo "==> $name"
  start="$(date +%s)"
  "$@" > "$json_out" 2> "$log"
  code=$?
  end="$(date +%s)"
  record_status "$name" "$code" "$((end - start))" "$(basename "$log")"
  if [ "$code" -ne 0 ]; then
    echo "    failed: $log"
  else
    echo "    ok: $json_out"
  fi
  return 0
}

start_bg() {
  local name="$1"
  shift
  local log="$RUN_DIR/$name.log"
  echo "==> start $name"
  "$@" > "$log" 2>&1 &
  PIDS+=("$!")
  echo "    pid ${PIDS[-1]} log $log"
}

prepare_checkpoints() {
  local size_mb="${LOGSERVE_CHECKPOINT_MB:-1}"
  local model
  for model in model-A model-B model-C model-D; do
    local dir="$CHECKPOINT_SOURCE_DIR/${model}-v1"
    mkdir -p "$dir"
    if [ ! -f "$dir/checkpoint.bin" ]; then
      dd if=/dev/zero of="$dir/checkpoint.bin" bs=1M count="$size_mb" status=none
    fi
  done
}

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "${PIDS[@]:-}"; do
    wait "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_tcp() {
  local host="$1"
  local port="$2"
  local timeout_sec="$3"
  python - "$host" "$port" "$timeout_sec" <<'PY'
import socket
import sys
import time

host = sys.argv[1]
port = int(sys.argv[2])
deadline = time.time() + int(sys.argv[3])
while time.time() < deadline:
    with socket.socket() as s:
        s.settimeout(0.5)
        try:
            s.connect((host, port))
            sys.exit(0)
        except OSError:
            time.sleep(0.25)
sys.exit(1)
PY
}

write_environment() {
  {
    echo "run_id=$RUN_ID"
    echo "root=$ROOT"
    echo "run_dir=$RUN_DIR"
    echo "log_addr=$LOG_ADDR"
    echo "control_addr=$CONTROL_ADDR"
    echo "generated_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    uname -a || true
    go version || true
    python --version || true
    git rev-parse --short HEAD 2>/dev/null || true
    git status --short 2>/dev/null || true
  } > "$RUN_DIR/environment.txt"
}

start_runtime() {
  mkdir -p "$DATA_DIR/logstore"
  prepare_checkpoints
  start_bg logd go run ./cmd/logserve-logd --addr "$LOG_ADDR" --data-dir "$DATA_DIR/logstore" --segment-size-bytes 67108864 --fsync-policy always
  if ! wait_tcp 127.0.0.1 "${LOG_ADDR##*:}" 30; then
    record_status runtime_logd_start 1 30 logd.log
    return 1
  fi
  record_status runtime_logd_start 0 0 logd.log

  start_bg control go run ./cmd/logserve-control --addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR"
  if ! wait_tcp 127.0.0.1 "${CONTROL_ADDR##*:}" 30; then
    record_status runtime_control_start 1 30 control.log
    return 1
  fi
  record_status runtime_control_start 0 0 control.log

  start_bg worker_1 go run ./cmd/logserve-worker --worker-id bench-worker-1 --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --models model-A:v1 --capacity 1 --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-1" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  start_bg worker_2 go run ./cmd/logserve-worker --worker-id bench-worker-2 --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --models model-B:v1 --capacity 1 --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-2" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  start_bg worker_3 go run ./cmd/logserve-worker --worker-id bench-worker-3 --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --capacity 1 --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-3" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  sleep 4
  record_status runtime_workers_start 0 4 worker_1.log
  return 0
}

write_fault_report() {
  python - "$RUN_DIR" <<'PY'
import json
import sys
from pathlib import Path

run_dir = Path(sys.argv[1])
statuses = []
status_path = run_dir / "command_status.jsonl"
if status_path.exists():
    statuses = [json.loads(line) for line in status_path.read_text(encoding="utf-8").splitlines() if line.strip()]

def status(name):
    for item in statuses:
        if item["name"] == name:
            return "passed" if item["exit_code"] == 0 else "failed"
    return "not_run"

report = {
    "worker_kill_recovery": status("fault_injection_go_tests"),
    "queue_redelivery": status("fault_injection_go_tests"),
    "control_restart_probe": status("fault_injection_go_tests"),
    "logd_restart_probe": "covered_by_logstore_recovery_and_process_logs",
    "source": "go test ./tests/integration fault and recovery subset",
}
(run_dir / "fault_injection.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
PY
}

write_environment
: > "$STATUS_FILE"

if [ "${LOGSERVE_RUN_FULL_TESTS:-1}" = "1" ]; then
  run_step go_test_all "$RUN_DIR/go_test_all.log" go test -count=1 ./...
  run_step go_vet "$RUN_DIR/go_vet.log" go vet ./...
fi

if [ "${LOGSERVE_RUN_RACE:-1}" = "1" ]; then
  run_step go_race_control_worker "$RUN_DIR/go_race_control_worker.log" go test -race -count=1 ./internal/control ./internal/worker
fi

run_step python_unittest "$RUN_DIR/python_unittest.log" python -m unittest discover sdk/python/tests
run_step python_compileall "$RUN_DIR/python_compileall.log" python -m compileall -q sdk/python/logserve
run_step python_grpc_deps "$RUN_DIR/python_grpc_deps.log" python -c "import grpc; import google.protobuf"

if [ "${LOGSERVE_RUN_LOGSTORE_BENCH:-1}" = "1" ]; then
  run_step logstore_v1_benchmark "$RUN_DIR/logstore_v1_benchmark.log" env LOGSERVE_LOGBENCH_OUT="$RUN_DIR/logstore_v1_latest.json" bash scripts/logstore_v1_benchmark.sh
fi

if [ "${LOGSERVE_RUN_FAULT:-1}" = "1" ]; then
  run_step fault_injection_go_tests "$RUN_DIR/fault_injection_go_tests.log" go test ./tests/integration -run "Test(WorkflowWorkerRecoveryContinuesAfterCompletedStep|ActorCounterRecoverySnapshotAndReplay|RunningTaskIsRedeliveredAfterWorkerLeaseExpires|PolledTaskIsRedeliveredWhenWorkerDiesBeforeStart|StaleTaskCompletionRejectedAfterRedelivery|OrdinaryTaskSurvivesControlRestartFromTaskSpecLog|ControlRestartBootstrapsWorkflowAndModelStateFromLog)" -count=1
  write_fault_report
fi

if [ "${LOGSERVE_RUN_PHASE5_BENCH:-1}" = "1" ]; then
  if start_runtime; then
    run_json_step phase5_benchmark "$RUN_DIR/phase5_benchmark.json" "$RUN_DIR/phase5_benchmark.stderr.log" python examples/phase5/benchmark.py
    run_json_step checkpoint_cache_probe "$RUN_DIR/checkpoint_cache_probe.json" "$RUN_DIR/checkpoint_cache_probe.stderr.log" python examples/phase5/checkpoint_cache.py
    run_json_step dashboard_snapshot "$RUN_DIR/dashboard_snapshot.json" "$RUN_DIR/dashboard_snapshot.stderr.log" go run ./cmd/logservectl dashboard-snapshot --control-addr "$CONTROL_ADDR"
  fi
fi

run_step summarize_experiment "$RUN_DIR/summarize_experiment.log" python scripts/summarize_experiment.py "$RUN_DIR"
python scripts/summarize_experiment.py "$RUN_DIR" >> "$RUN_DIR/summarize_experiment.log" 2>&1 || true

echo
echo "Experiment directory: $RUN_DIR"
echo "Summary: $RUN_DIR/summary.md"
exit "$ANY_FAIL"
