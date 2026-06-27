#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_EXPERIMENT_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_EXPERIMENT_ID"
RUN_DIR="${LOGSERVE_EXPERIMENT_DIR:-"$ROOT/reports/compose-experiment-$RUN_ID"}"
logserve_reject_dated_name "$RUN_DIR" "LOGSERVE_EXPERIMENT_DIR"
STATUS_FILE="$RUN_DIR/command_status.jsonl"
DATA_DIR="${LOGSERVE_EXPERIMENT_DATA_DIR:-"$RUN_DIR/runtime"}"
EXPERIMENT_MODE="${LOGSERVE_EXPERIMENT_MODE:-compose}"
PYTHON_BOOTSTRAP="${PYTHON:-python3}"
PYTHON="$PYTHON_BOOTSTRAP"
CLI_BIN="$RUN_DIR/bin/logservectl"
PACKAGE_PATH="$RUN_DIR/experiment-package.tar.gz"
COMPOSE_ENV="$RUN_DIR/compose.env"
COMPOSE_PROJECT="logserve-exp-$(printf '%s' "$RUN_ID" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9_-')"
SCHEDULER_V2="${LOGSERVE_SCHEDULER_V2:-1}"
POSTGRES_MODE="${LOGSERVE_POSTGRES_MODE:-sync}"

PORTS="$("$PYTHON_BOOTSTRAP" - <<'PY'
import socket

sockets = []
ports = []
for _ in range(6):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sockets.append(sock)
    ports.append(sock.getsockname()[1])
print(" ".join(str(port) for port in ports))
for sock in sockets:
    sock.close()
PY
)"
read -r LOG_PORT CONTROL_PORT POSTGRES_PORT NATS_PORT MINIO_API_PORT MINIO_CONSOLE_PORT <<< "$PORTS"
LOG_ADDR="${LOGSERVE_EXPERIMENT_LOG_ADDR:-127.0.0.1:$LOG_PORT}"
CONTROL_ADDR="${LOGSERVE_EXPERIMENT_CONTROL_ADDR:-127.0.0.1:$CONTROL_PORT}"
CHECKPOINT_SOURCE_DIR="${LOGSERVE_CHECKPOINT_SOURCE_DIR:-"$DATA_DIR/checkpoints"}"
CHECKPOINT_CACHE_BYTES="${LOGSERVE_CHECKPOINT_CACHE_BYTES:-16777216}"
WORKER_CAPACITY="${LOGSERVE_WORKER_CAPACITY:-1}"
TASK_POOL_SIZE="${LOGSERVE_TASK_POOL_SIZE:-0}"
LLM_POOL_SIZE="${LOGSERVE_LLM_POOL_SIZE:-0}"
ACTOR_POOL_SIZE="${LOGSERVE_ACTOR_POOL_SIZE:-0}"
API_TOKEN="${LOGSERVE_API_TOKEN:-"logserve-experiment-$RUN_ID"}"
POSTGRES_PASSWORD="${LOGSERVE_POSTGRES_PASSWORD:-"logserve-postgres-$RUN_ID"}"
S3_ACCESS_KEY="${LOGSERVE_S3_ACCESS_KEY:-logserve}"
S3_SECRET_KEY="${LOGSERVE_S3_SECRET_KEY:-"logserve-minio-$RUN_ID"}"

mkdir -p "$RUN_DIR" "$DATA_DIR" "$RUN_DIR/bin"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

export GOCACHE="${GOCACHE:-"$ROOT/.gocache"}"
export PYTHONPATH="${PYTHONPATH:-"$ROOT/sdk/python"}"
export LOGSERVE_SDK_TRANSPORT="${LOGSERVE_SDK_TRANSPORT:-grpc}"
export LOGSERVE_CONTROL_ADDR="$CONTROL_ADDR"



PIDS=()
LAST_BG_PID=""
ANY_FAIL=0
LAST_STEP_CODE=0
COMPOSE_STARTED=0
COMPOSE_AVAILABLE=0
COMPOSE_CMD=()

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
  LAST_STEP_CODE="$code"
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
  LAST_STEP_CODE="$code"
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
  LAST_BG_PID="$!"
  PIDS+=("$LAST_BG_PID")
  echo "    pid $LAST_BG_PID log $log"
}

ensure_bg_alive() {
  local name="$1"
  local pid="$2"
  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi
  echo "$name process exited unexpectedly"
  tail -n 80 "$RUN_DIR/$name.log" 2>/dev/null || true
  return 1
}

detect_compose() {
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD=(docker compose)
    COMPOSE_AVAILABLE=1
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD=(docker-compose)
    COMPOSE_AVAILABLE=1
    return 0
  fi
  COMPOSE_AVAILABLE=0
  return 1
}

compose() {
  "${COMPOSE_CMD[@]}" \
    --env-file "$COMPOSE_ENV" \
    -p "$COMPOSE_PROJECT" \
    -f deployments/docker-compose.yml \
    -f deployments/docker-compose.experiment.yml \
    "$@"
}

cleanup() {
  if [ "$COMPOSE_STARTED" -eq 1 ] && [ "$COMPOSE_AVAILABLE" -eq 1 ]; then
    compose logs --no-color > "$RUN_DIR/compose.log" 2>&1 || true
    compose down --remove-orphans --volumes > "$RUN_DIR/compose_down.log" 2>&1 || true
  fi
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
  "$PYTHON_BOOTSTRAP" - "$host" "$port" "$timeout_sec" <<'PY'
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

prepare_runtime_dirs() {
  mkdir -p "$DATA_DIR/logstore" "$DATA_DIR/model-cache" "$CHECKPOINT_SOURCE_DIR"
  prepare_checkpoints
  chmod -R a+rwX "$DATA_DIR" 2>/dev/null || true
}

write_compose_env() {
  cat > "$COMPOSE_ENV" <<EOF
LOGSERVE_API_TOKEN=$API_TOKEN
LOGSERVE_SCHEDULER_V2=$SCHEDULER_V2
LOGSERVE_POSTGRES_MODE=$POSTGRES_MODE
LOGSERVE_DOCKER_GOPROXY=${LOGSERVE_DOCKER_GOPROXY:-https://goproxy.cn,direct}
LOGSERVE_DOCKER_GOSUMDB=${LOGSERVE_DOCKER_GOSUMDB:-sum.golang.org}
LOGSERVE_POSTGRES_USER=logserve
LOGSERVE_POSTGRES_PASSWORD=$POSTGRES_PASSWORD
LOGSERVE_POSTGRES_DB=logserve
LOGSERVE_POSTGRES_PORT=$POSTGRES_PORT
LOGSERVE_NATS_PORT=$NATS_PORT
LOGSERVE_MINIO_API_PORT=$MINIO_API_PORT
LOGSERVE_MINIO_CONSOLE_PORT=$MINIO_CONSOLE_PORT
LOGSERVE_LOGD_PORT=${LOG_ADDR##*:}
LOGSERVE_CONTROL_PORT=${CONTROL_ADDR##*:}
LOGSERVE_S3_ACCESS_KEY=$S3_ACCESS_KEY
LOGSERVE_S3_SECRET_KEY=$S3_SECRET_KEY
LOGSERVE_S3_BUCKET=logserve-results
LOGSERVE_S3_REGION=us-east-1
LOGSERVE_WORKER_CAPACITY=$WORKER_CAPACITY
LOGSERVE_TASK_POOL_SIZE=$TASK_POOL_SIZE
LOGSERVE_LLM_POOL_SIZE=$LLM_POOL_SIZE
LOGSERVE_ACTOR_POOL_SIZE=$ACTOR_POOL_SIZE
LOGSERVE_CHECKPOINT_CACHE_BYTES=$CHECKPOINT_CACHE_BYTES
LOGSERVE_CHECKPOINT_SOURCE_DIR=$CHECKPOINT_SOURCE_DIR
LOGSERVE_EXPERIMENT_MODEL_CACHE_DIR=$DATA_DIR/model-cache
LOGSERVE_EXPERIMENT_LOGSTORE_DIR=$DATA_DIR/logstore
EOF
}

write_environment() {
  {
    echo "run_id=$RUN_ID"
    echo "mode=$EXPERIMENT_MODE"
    echo "postgres_mode=$POSTGRES_MODE"
    echo "root=$ROOT"
    echo "run_dir=$RUN_DIR"
    echo "data_dir=$DATA_DIR"
    echo "log_addr=$LOG_ADDR"
    echo "control_addr=$CONTROL_ADDR"
    echo "postgres_port=$POSTGRES_PORT"
    echo "nats_port=$NATS_PORT"
    echo "minio_api_port=$MINIO_API_PORT"
    echo "minio_console_port=$MINIO_CONSOLE_PORT"
    echo "checkpoint_source_dir=$CHECKPOINT_SOURCE_DIR"
    echo "checkpoint_cache_bytes=$CHECKPOINT_CACHE_BYTES"
    echo "worker_capacity=$WORKER_CAPACITY"
    echo "task_pool_size=$TASK_POOL_SIZE"
    echo "llm_pool_size=$LLM_POOL_SIZE"
    echo "actor_pool_size=$ACTOR_POOL_SIZE"
    echo "generated_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    uname -a || true
    go version || true
    "$PYTHON_BOOTSTRAP" --version || true
    docker --version 2>/dev/null || true
    if detect_compose; then "${COMPOSE_CMD[@]}" version || true; fi
    git rev-parse --short HEAD 2>/dev/null || true
    git status --short 2>/dev/null || true
  } > "$RUN_DIR/environment.txt"
}

verify_checkpoint_cache_artifact() {
  local model="${LOGSERVE_CHECKPOINT_MODEL:-model-D}"
  local version="${LOGSERVE_CHECKPOINT_VERSION:-v1}"
  local checkpoint_name="${model}-${version}.checkpoint"
  local matches
  matches="$(find "$DATA_DIR/model-cache" -type f -name "$checkpoint_name" 2>/dev/null || true)"
  if [ -z "$matches" ]; then
    echo "missing local checkpoint cache artifact: $checkpoint_name under $DATA_DIR/model-cache"
    echo
    echo "existing model-cache files:"
    find "$DATA_DIR/model-cache" -maxdepth 4 -type f -print 2>/dev/null || true
    return 1
  fi
  echo "$matches"
  return 0
}

wait_dashboard_workers() {
  local want="${1:-3}"
  local timeout_sec="${2:-90}"
  local deadline now tmp count
  tmp="$RUN_DIR/dashboard_wait.json"
  deadline=$(( $(date +%s) + timeout_sec ))
  while true; do
    "$CLI_BIN" dashboard-snapshot --control-addr "$CONTROL_ADDR" > "$tmp" 2> "$RUN_DIR/dashboard_wait.stderr.log"
    if [ "$?" -eq 0 ]; then
      count="$("$PYTHON_BOOTSTRAP" - "$tmp" <<'PY'
import json
import sys
from pathlib import Path

try:
    data = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8-sig"))
except Exception:
    print(0)
else:
    print(len(data.get("workers") or []))
PY
)"
      if [ "$count" -ge "$want" ]; then
        return 0
      fi
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      echo "dashboard worker count did not reach $want"
      cat "$tmp" 2>/dev/null || true
      return 1
    fi
    sleep 2
  done
}

postgres_stats_json() {
  if [ "$EXPERIMENT_MODE" != "compose" ] || [ "$COMPOSE_STARTED" -ne 1 ] || [ "$COMPOSE_AVAILABLE" -ne 1 ]; then
    printf '{"available":false,"reason":"postgres stats require compose runtime","mode":"%s"}\n' "$POSTGRES_MODE"
    return 0
  fi
  compose exec -T postgres env PGPASSWORD="$POSTGRES_PASSWORD" psql -U logserve -d logserve -tAc "select json_build_object('available', true, 'mode', '$POSTGRES_MODE', 'captured_at_ms', floor(extract(epoch from clock_timestamp()) * 1000)::bigint, 'datname', datname, 'xact_commit', xact_commit, 'xact_rollback', xact_rollback, 'tup_inserted', tup_inserted, 'tup_updated', tup_updated, 'tup_deleted', tup_deleted, 'tup_returned', tup_returned, 'tup_fetched', tup_fetched) from pg_stat_database where datname = current_database();"
}

postgres_benchmark_delta_json() {
  "$PYTHON_BOOTSTRAP" - "$RUN_DIR/postgres_before_benchmark.json" "$RUN_DIR/postgres_after_benchmark.json" <<'PY'
import json
import sys
from pathlib import Path

before = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8-sig"))
after = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8-sig"))
if not before.get("available") or not after.get("available"):
    print(json.dumps({"available": False, "reason": "postgres stats unavailable", "before": before, "after": after}, indent=2))
    sys.exit(0)

def num(data, key):
    value = data.get(key, 0)
    return float(value or 0)

elapsed_ms = max(1.0, num(after, "captured_at_ms") - num(before, "captured_at_ms"))
elapsed_sec = elapsed_ms / 1000.0
xact_delta = (num(after, "xact_commit") + num(after, "xact_rollback")) - (num(before, "xact_commit") + num(before, "xact_rollback"))
row_writes_delta = sum(num(after, key) - num(before, key) for key in ("tup_inserted", "tup_updated", "tup_deleted"))
print(json.dumps({
    "available": True,
    "mode": after.get("mode") or before.get("mode"),
    "elapsed_ms": int(elapsed_ms),
    "transactions_delta": int(xact_delta),
    "row_writes_delta": int(row_writes_delta),
    "transactions_per_sec": round(xact_delta / elapsed_sec, 3),
    "row_writes_per_sec": round(row_writes_delta / elapsed_sec, 3),
    "before": before,
    "after": after,
}, indent=2))
PY
}

verify_dashboard_replay_consistency() {
  "$PYTHON_BOOTSTRAP" - "$CLI_BIN" "$CONTROL_ADDR" "$RUN_DIR/dashboard_snapshot.json" <<'PY'
import json
import os
import subprocess
import sys
from pathlib import Path

cli, control_addr, dashboard_path = sys.argv[1:4]
dashboard = json.loads(Path(dashboard_path).read_text(encoding="utf-8-sig"))
failures = []
workflow_count = 0
actor_count = 0

def pick(data, snake, camel):
    return data.get(snake) if snake in data else data.get(camel)

def run_json(args):
    proc = subprocess.run(args, text=True, capture_output=True, env=os.environ)
    if proc.returncode != 0:
        return None, proc.stderr.strip() or proc.stdout.strip() or f"exit {proc.returncode}"
    try:
        return json.loads(proc.stdout), ""
    except json.JSONDecodeError as exc:
        return None, f"invalid json: {exc}"

for workflow in dashboard.get("workflows") or []:
    workflow_id = pick(workflow, "workflow_id", "workflowId")
    if not workflow_id:
        continue
    workflow_count += 1
    data, err = run_json([cli, "workflow-replay", "--control-addr", control_addr, "--workflow-id", workflow_id])
    if err or not data or not data.get("consistent_with_metadata"):
        failures.append({"kind": "workflow", "id": workflow_id, "error": err, "consistent": bool(data and data.get("consistent_with_metadata"))})

for actor in dashboard.get("actors") or []:
    actor_id = pick(actor, "actor_id", "actorId")
    if not actor_id:
        continue
    actor_count += 1
    data, err = run_json([cli, "actor-replay", "--control-addr", control_addr, "--actor-id", actor_id])
    if err or not data or not data.get("consistent_with_metadata"):
        failures.append({"kind": "actor", "id": actor_id, "error": err, "consistent": bool(data and data.get("consistent_with_metadata"))})

out = {
    "consistent": not failures,
    "workflow_count": workflow_count,
    "actor_count": actor_count,
    "checked_count": workflow_count + actor_count,
    "failures": failures,
}
print(json.dumps(out, indent=2))
sys.exit(0 if not failures else 1)
PY
}
start_native_runtime() {
  prepare_runtime_dirs
  start_bg logd env LOGSERVE_API_TOKEN="$API_TOKEN" go run ./cmd/logserve-logd --addr "$LOG_ADDR" --data-dir "$DATA_DIR/logstore" --segment-size-bytes 67108864 --fsync-policy always
  local logd_pid="$LAST_BG_PID"
  if ! wait_tcp 127.0.0.1 "${LOG_ADDR##*:}" 30 || ! ensure_bg_alive logd "$logd_pid"; then
    record_status runtime_logd_start 1 30 logd.log
    return 1
  fi
  record_status runtime_logd_start 0 0 logd.log

  local control_pprof_args=()
  if [ -n "${LOGSERVE_CONTROL_PPROF_ADDR:-}" ]; then
    control_pprof_args=(--pprof-addr "$LOGSERVE_CONTROL_PPROF_ADDR")
  fi
  start_bg control env LOGSERVE_API_TOKEN="$API_TOKEN" LOGSERVE_SCHEDULER_V2="$SCHEDULER_V2" go run ./cmd/logserve-control --addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" "${control_pprof_args[@]}"
  local control_pid="$LAST_BG_PID"
  if ! wait_tcp 127.0.0.1 "${CONTROL_ADDR##*:}" 30 || ! ensure_bg_alive control "$control_pid"; then
    record_status runtime_control_start 1 30 control.log
    return 1
  fi
  record_status runtime_control_start 0 0 control.log

  start_bg worker_a env LOGSERVE_API_TOKEN="$API_TOKEN" go run ./cmd/logserve-worker --worker-id worker-a --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --models model-A:v1 --capacity "$WORKER_CAPACITY" --task-pool-size "$TASK_POOL_SIZE" --llm-pool-size "$LLM_POOL_SIZE" --actor-pool-size "$ACTOR_POOL_SIZE" --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-a" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  local worker_a_pid="$LAST_BG_PID"
  start_bg worker_b env LOGSERVE_API_TOKEN="$API_TOKEN" go run ./cmd/logserve-worker --worker-id worker-b --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --models model-B:v1 --capacity "$WORKER_CAPACITY" --task-pool-size "$TASK_POOL_SIZE" --llm-pool-size "$LLM_POOL_SIZE" --actor-pool-size "$ACTOR_POOL_SIZE" --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-b" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  local worker_b_pid="$LAST_BG_PID"
  start_bg worker_c env LOGSERVE_API_TOKEN="$API_TOKEN" go run ./cmd/logserve-worker --worker-id worker-c --control-addr "$CONTROL_ADDR" --log-addr "$LOG_ADDR" --executor "$ROOT/executor/python/server.py" --capacity "$WORKER_CAPACITY" --task-pool-size "$TASK_POOL_SIZE" --llm-pool-size "$LLM_POOL_SIZE" --actor-pool-size "$ACTOR_POOL_SIZE" --model-source-dir "$CHECKPOINT_SOURCE_DIR" --model-cache-dir "$DATA_DIR/model-cache/worker-c" --model-cache-capacity-bytes "$CHECKPOINT_CACHE_BYTES"
  local worker_c_pid="$LAST_BG_PID"
  sleep 4
  local worker_start_code=0
  ensure_bg_alive worker_a "$worker_a_pid" || worker_start_code=1
  ensure_bg_alive worker_b "$worker_b_pid" || worker_start_code=1
  ensure_bg_alive worker_c "$worker_c_pid" || worker_start_code=1
  record_status runtime_workers_start "$worker_start_code" 4 worker_a.log
  return "$worker_start_code"
}

start_compose_runtime() {
  prepare_runtime_dirs
  write_compose_env
  if ! detect_compose; then
    echo "docker compose is required for LOGSERVE_EXPERIMENT_MODE=compose"
    record_status runtime_compose_start 1 0 compose.log
    return 1
  fi
  run_step compose_build "$RUN_DIR/compose_build.log" compose build
  if [ "$LAST_STEP_CODE" -ne 0 ]; then
    return 1
  fi
  echo "==> start compose runtime"
  local start end code
  start="$(date +%s)"
  compose up -d postgres nats minio logd control worker-a worker-b worker-c > "$RUN_DIR/compose_up.log" 2>&1
  code=$?
  LAST_STEP_CODE="$code"
  end="$(date +%s)"
  COMPOSE_STARTED=1
  record_status runtime_compose_start "$code" "$((end - start))" compose_up.log
  if [ "$code" -ne 0 ]; then
    return 1
  fi
  if ! wait_tcp 127.0.0.1 "${LOG_ADDR##*:}" 60; then
    record_status runtime_logd_ready 1 60 compose.log
    return 1
  fi
  record_status runtime_logd_ready 0 0 compose.log
  if ! wait_tcp 127.0.0.1 "${CONTROL_ADDR##*:}" 90; then
    record_status runtime_control_ready 1 90 compose.log
    return 1
  fi
  record_status runtime_control_ready 0 0 compose.log
  if wait_dashboard_workers 3 120 > "$RUN_DIR/runtime_workers_ready.log" 2>&1; then
    record_status runtime_workers_ready 0 0 runtime_workers_ready.log
    return 0
  fi
  record_status runtime_workers_ready 1 120 runtime_workers_ready.log
  return 1
}

start_runtime() {
  case "$EXPERIMENT_MODE" in
    compose)
      start_compose_runtime
      ;;
    native)
      start_native_runtime
      ;;
    *)
      echo "unsupported LOGSERVE_EXPERIMENT_MODE=$EXPERIMENT_MODE; use compose or native"
      record_status runtime_mode 1 0 environment.txt
      return 1
      ;;
  esac
}

write_fault_report() {
  "$PYTHON_BOOTSTRAP" - "$RUN_DIR" <<'PY'
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

setup_python() {
  if [ "${LOGSERVE_USE_VENV:-1}" != "1" ]; then
    PYTHON="$PYTHON_BOOTSTRAP"
    return 0
  fi
  run_step python_venv_create "$RUN_DIR/python_venv_create.log" "$PYTHON_BOOTSTRAP" -m venv "$RUN_DIR/venv"
  PYTHON="$RUN_DIR/venv/bin/python"
  if [ ! -x "$PYTHON" ]; then
    PYTHON="$PYTHON_BOOTSTRAP"
    return 1
  fi
  run_step python_pip_install "$RUN_DIR/python_pip_install.log" "$PYTHON" -m pip install -r sdk/python/requirements.txt
  return 0
}

package_results() {
  local tar_excludes=(--exclude ./experiment-package.tar.gz --exclude ./venv)
  local tmp_package="$RUN_DIR/../.$(basename "$RUN_DIR").package.tmp.tar.gz"
  if [ "${LOGSERVE_EXPERIMENT_KEEP_RUNTIME:-0}" != "1" ]; then
    tar_excludes+=(--exclude ./runtime)
  fi
  rm -f "$tmp_package" "$PACKAGE_PATH"
  tar "${tar_excludes[@]}" -czf "$tmp_package" -C "$RUN_DIR" . > "$RUN_DIR/package.log" 2>&1
  local code=$?
  if [ "$code" -eq 0 ]; then
    mv "$tmp_package" "$PACKAGE_PATH" >> "$RUN_DIR/package.log" 2>&1
    code=$?
  else
    rm -f "$tmp_package"
  fi
  record_status package_results "$code" 0 package.log
  return "$code"
}

write_environment
: > "$STATUS_FILE"
setup_python
HOST_GO_ENV=(env -u LOGSERVE_API_TOKEN -u LOGSERVE_SCHEDULER_V2)

if [ "${LOGSERVE_RUN_FULL_TESTS:-1}" = "1" ]; then
  run_step go_test_all "$RUN_DIR/go_test_all.log" "${HOST_GO_ENV[@]}" go test -count=1 ./...
  run_step go_vet "$RUN_DIR/go_vet.log" "${HOST_GO_ENV[@]}" go vet ./...
fi

if [ "${LOGSERVE_RUN_RACE:-1}" = "1" ]; then
  run_step go_race_control_metadata_worker "$RUN_DIR/go_race_control_metadata_worker.log" "${HOST_GO_ENV[@]}" go test -race -count=1 ./internal/control ./internal/metadata ./internal/worker
fi

run_step python_unittest "$RUN_DIR/python_unittest.log" "$PYTHON" -m unittest discover sdk/python/tests
run_step python_compileall "$RUN_DIR/python_compileall.log" "$PYTHON" -m compileall -q sdk/python/logserve
run_step python_grpc_deps "$RUN_DIR/python_grpc_deps.log" "$PYTHON" -c "import grpc; import google.protobuf"

run_step build_logservectl "$RUN_DIR/build_logservectl.log" go build -o "$CLI_BIN" ./cmd/logservectl

if [ "${LOGSERVE_RUN_GO_BENCH:-1}" = "1" ]; then
  run_step scheduler_benchmark "$RUN_DIR/scheduler_benchmark.log" "${HOST_GO_ENV[@]}" go test ./internal/control -run '^$' -bench 'BenchmarkSchedulerAssignMixedBacklog|BenchmarkPreferred' -benchmem -benchtime "${LOGSERVE_GO_BENCHTIME:-300ms}"
  run_step logstore_micro_benchmark "$RUN_DIR/logstore_micro_benchmark.log" "${HOST_GO_ENV[@]}" go test ./internal/logstore -run '^$' -bench 'BenchmarkStoreAppend|BenchmarkStoreRecover|BenchmarkRead' -benchmem -benchtime "${LOGSERVE_GO_BENCHTIME:-300ms}"
  run_step bootstrap_micro_benchmark "$RUN_DIR/bootstrap_micro_benchmark.log" "${HOST_GO_ENV[@]}" go test ./internal/control -run '^$' -bench BenchmarkBootstrapFromLog -benchmem -benchtime "${LOGSERVE_GO_BENCHTIME:-300ms}"
  run_step workflow_micro_benchmark "$RUN_DIR/workflow_micro_benchmark.log" "${HOST_GO_ENV[@]}" go test ./internal/workflow -run '^$' -bench 'BenchmarkSchedule' -benchmem -benchtime "${LOGSERVE_GO_BENCHTIME:-300ms}"
  run_step metadata_benchmark "$RUN_DIR/metadata_benchmark.log" "${HOST_GO_ENV[@]}" go test ./internal/metadata -run '^$' -bench BenchmarkMemoryStore -benchmem -benchtime "${LOGSERVE_GO_BENCHTIME:-300ms}" -memprofile "$RUN_DIR/metadata_heap.pprof" -mutexprofile "$RUN_DIR/metadata_mutex.pprof" -blockprofile "$RUN_DIR/metadata_block.pprof"
fi

if [ "${LOGSERVE_RUN_LOGSTORE_BENCH:-1}" = "1" ]; then
  run_step logstore_benchmark "$RUN_DIR/logstore_benchmark.log" env LOGSERVE_LOGBENCH_OUT="$RUN_DIR/logstore_latest.json" bash scripts/logstore_benchmark.sh
fi

if [ "${LOGSERVE_RUN_FAULT:-1}" = "1" ]; then
  run_step fault_injection_go_tests "$RUN_DIR/fault_injection_go_tests.log" "${HOST_GO_ENV[@]}" go test ./tests/integration -run "Test(WorkflowWorkerRecoveryContinuesAfterCompletedStep|ActorCounterRecoverySnapshotAndReplay|RunningTaskIsRedeliveredAfterWorkerLeaseExpires|PolledTaskIsRedeliveredWhenWorkerDiesBeforeStart|StaleTaskCompletionRejectedAfterRedelivery|OrdinaryTaskSurvivesControlRestartFromTaskSpecLog|ControlRestartBootstrapsWorkflowAndModelStateFromLog)" -count=1
  write_fault_report
fi

if [ "${LOGSERVE_RUN_BENCHMARK:-1}" = "1" ]; then
  export LOGSERVE_API_TOKEN="$API_TOKEN"
  export LOGSERVE_SCHEDULER_V2="$SCHEDULER_V2"
  export LOGSERVE_POSTGRES_MODE="$POSTGRES_MODE"
  if start_runtime; then
    run_json_step postgres_before_benchmark "$RUN_DIR/postgres_before_benchmark.json" "$RUN_DIR/postgres_before_benchmark.stderr.log" postgres_stats_json
    run_json_step benchmark "$RUN_DIR/benchmark.json" "$RUN_DIR/benchmark.stderr.log" "$PYTHON" examples/evaluation/benchmark.py
    run_json_step postgres_after_benchmark "$RUN_DIR/postgres_after_benchmark.json" "$RUN_DIR/postgres_after_benchmark.stderr.log" postgres_stats_json
    run_json_step postgres_benchmark_stats "$RUN_DIR/postgres_benchmark_stats.json" "$RUN_DIR/postgres_benchmark_stats.stderr.log" postgres_benchmark_delta_json
    run_json_step checkpoint_cache_probe "$RUN_DIR/checkpoint_cache_probe.json" "$RUN_DIR/checkpoint_cache_probe.stderr.log" "$PYTHON" examples/evaluation/checkpoint_cache.py
    run_json_step checkpoint_cache_bench "$RUN_DIR/checkpoint_cache_bench.json" "$RUN_DIR/checkpoint_cache_bench.stderr.log" "$PYTHON" examples/evaluation/checkpoint_cache_bench.py
    run_json_step executor_bench "$RUN_DIR/executor_bench.json" "$RUN_DIR/executor_bench.stderr.log" "$PYTHON" examples/evaluation/executor_bench.py
    if [ -n "${LOGSERVE_PPROF_ADDR:-${LOGSERVE_CONTROL_PPROF_ADDR:-}}" ]; then
      run_step collect_pprof "$RUN_DIR/collect_pprof.log" env LOGSERVE_PPROF_OUT="$RUN_DIR/profiles" bash scripts/collect_pprof.sh "${LOGSERVE_PPROF_ADDR:-$LOGSERVE_CONTROL_PPROF_ADDR}"
    fi
    run_step checkpoint_cache_artifact "$RUN_DIR/checkpoint_cache_artifact.log" verify_checkpoint_cache_artifact
    run_json_step dashboard_snapshot "$RUN_DIR/dashboard_snapshot.json" "$RUN_DIR/dashboard_snapshot.stderr.log" "$CLI_BIN" dashboard-snapshot --control-addr "$CONTROL_ADDR"
    run_json_step dashboard_replay_consistency "$RUN_DIR/dashboard_replay_consistency.json" "$RUN_DIR/dashboard_replay_consistency.stderr.log" verify_dashboard_replay_consistency
  fi
fi

run_step summarize_experiment "$RUN_DIR/summarize_experiment.log" "$PYTHON_BOOTSTRAP" scripts/summarize_experiment.py "$RUN_DIR"
package_results
"$PYTHON_BOOTSTRAP" scripts/summarize_experiment.py "$RUN_DIR" >> "$RUN_DIR/summarize_experiment.log" 2>&1 || true

echo
echo "Experiment directory: $RUN_DIR"
echo "Summary: $RUN_DIR/summary.md"
echo "Package: $PACKAGE_PATH"
exit "$ANY_FAIL"
