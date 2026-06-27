#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_CONSOLE_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_CONSOLE_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_CONSOLE_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-console-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_CONSOLE_ACCEPTANCE_DIR"
STATUS_FILE="$RESULT_DIR/command_status.jsonl"
PACKAGE_PATH="$RESULT_DIR/console-acceptance-package.tar.gz"
COMPOSE_ENV="$RESULT_DIR/console.env"
PYTHON_BOOTSTRAP="${PYTHON:-python3}"
NPM_CMD="${NPM:-npm}"
RUN_DOCKER="${LOGSERVE_CONSOLE_RUN_DOCKER:-1}"
RUN_NPM_CI="${LOGSERVE_CONSOLE_RUN_NPM_CI:-1}"
KEEP_STACK="${LOGSERVE_CONSOLE_KEEP_STACK:-0}"
API_TOKEN="${LOGSERVE_API_TOKEN:-"logserve-console-$RUN_ID"}"
POSTGRES_PASSWORD="${LOGSERVE_POSTGRES_PASSWORD:-"logserve-postgres-$RUN_ID"}"
S3_ACCESS_KEY="${LOGSERVE_S3_ACCESS_KEY:-logserve}"
S3_SECRET_KEY="${LOGSERVE_S3_SECRET_KEY:-"logserve-minio-$RUN_ID"}"
COMPOSE_PROJECT="logserve-console-$(printf '%s' "$RUN_ID" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9_-')"
ANY_FAIL=0
LAST_STEP_CODE=0
PREREQ_OK=1
SUMMARY_FAIL=0
COMPOSE_STARTED=0
COMPOSE_AVAILABLE=0
COMPOSE_CMD=()

mkdir -p "$RESULT_DIR"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

PORTS="$("$PYTHON_BOOTSTRAP" - <<'PY'
import socket

sockets = []
ports = []
for _ in range(7):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sockets.append(sock)
    ports.append(sock.getsockname()[1])
print(" ".join(str(port) for port in ports))
for sock in sockets:
    sock.close()
PY
)"
read -r LOG_PORT CONTROL_PORT WEB_PORT POSTGRES_PORT NATS_PORT MINIO_API_PORT MINIO_CONSOLE_PORT <<< "$PORTS"
BASE_URL="http://127.0.0.1:$WEB_PORT"

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

bool_enabled() {
  case "${1:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

detect_compose() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
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
    "$@"
}

compose_config_quiet() {
  compose config --quiet || compose config -q
}

write_server_environment() {
  {
    echo "run_id=$RUN_ID"
    echo "root=$ROOT"
    echo "result_dir=$RESULT_DIR"
    echo "base_url=$BASE_URL"
    echo "compose_project=$COMPOSE_PROJECT"
    echo "generated_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    uname -a || true
    if command -v lsb_release >/dev/null 2>&1; then lsb_release -a || true; fi
    if command -v nproc >/dev/null 2>&1; then echo "cpus=$(nproc)"; fi
    if [ -r /proc/meminfo ]; then grep -E '^(MemTotal|MemAvailable):' /proc/meminfo || true; fi
    echo
    go version || true
    node --version 2>/dev/null || true
    "$NPM_CMD" --version 2>/dev/null || true
    "$PYTHON_BOOTSTRAP" --version || true
    docker --version 2>/dev/null || true
    docker compose version 2>/dev/null || docker-compose --version 2>/dev/null || true
    git rev-parse --short HEAD 2>/dev/null || true
    git status --short 2>/dev/null || true
  } > "$RESULT_DIR/server_environment.txt"
}

write_run_config() {
  "$PYTHON_BOOTSTRAP" - "$RESULT_DIR/run_config.json" "$RUN_DOCKER" "$RUN_NPM_CI" "$BASE_URL" <<'PY'
import json
import sys
from pathlib import Path

def enabled(value):
    return str(value).lower() in {"1", "true", "yes", "on"}

path = Path(sys.argv[1])
config = {
    "run_docker": enabled(sys.argv[2]),
    "run_npm_ci": enabled(sys.argv[3]),
    "base_url": sys.argv[4],
}
path.write_text(json.dumps(config, indent=2), encoding="utf-8")
PY
}

write_compose_env() {
  cat > "$COMPOSE_ENV" <<EOF
LOGSERVE_API_TOKEN=$API_TOKEN
LOGSERVE_POSTGRES_USER=logserve
LOGSERVE_POSTGRES_PASSWORD=$POSTGRES_PASSWORD
LOGSERVE_POSTGRES_DB=logserve
LOGSERVE_POSTGRES_PORT=$POSTGRES_PORT
LOGSERVE_NATS_PORT=$NATS_PORT
LOGSERVE_MINIO_API_PORT=$MINIO_API_PORT
LOGSERVE_MINIO_CONSOLE_PORT=$MINIO_CONSOLE_PORT
LOGSERVE_LOGD_PORT=$LOG_PORT
LOGSERVE_CONTROL_PORT=$CONTROL_PORT
LOGSERVE_WEB_PORT=$WEB_PORT
LOGSERVE_S3_ACCESS_KEY=$S3_ACCESS_KEY
LOGSERVE_S3_SECRET_KEY=$S3_SECRET_KEY
LOGSERVE_S3_BUCKET=logserve-results
LOGSERVE_S3_REGION=us-east-1
LOGSERVE_DOCKER_GOPROXY=${LOGSERVE_DOCKER_GOPROXY:-https://goproxy.cn,direct}
LOGSERVE_DOCKER_GOSUMDB=${LOGSERVE_DOCKER_GOSUMDB:-sum.golang.org}
EOF
}

check_prerequisites() {
  local missing=0
  for cmd in bash git go tar "$PYTHON_BOOTSTRAP" "$NPM_CMD"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "missing required command: $cmd"
      missing=1
    fi
  done
  if bool_enabled "$RUN_DOCKER"; then
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
  fi
  return "$missing"
}

web_npm_ci() {
  (cd "$ROOT/web" && "$NPM_CMD" ci)
}

web_build() {
  (cd "$ROOT/web" && "$NPM_CMD" run build)
}

wait_web_health() {
  "$PYTHON_BOOTSTRAP" - "$BASE_URL" <<'PY'
import json
import sys
import time
import urllib.request

base = sys.argv[1].rstrip("/")
deadline = time.time() + 90
last_error = ""
while time.time() < deadline:
    try:
        with urllib.request.urlopen(base + "/api/healthz", timeout=2) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            if resp.status == 200 and data.get("status") == "ok":
                print(json.dumps(data, indent=2))
                sys.exit(0)
    except Exception as exc:
        last_error = str(exc)
    time.sleep(1)
print(last_error)
sys.exit(1)
PY
}

wait_console_api() {
  "$PYTHON_BOOTSTRAP" - "$BASE_URL" "$API_TOKEN" <<'PY'
import json
import sys
import time
import urllib.error
import urllib.request

base, token = sys.argv[1].rstrip("/"), sys.argv[2]
deadline = time.time() + 120
last_error = ""
while time.time() < deadline:
    req = urllib.request.Request(base + "/api/dashboard", headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            if resp.status == 200 and isinstance(data.get("workers"), list):
                print(json.dumps({"worker_count": len(data.get("workers") or []), "queue_depth": data.get("queue_depth")}, indent=2))
                sys.exit(0)
    except Exception as exc:
        last_error = str(exc)
    time.sleep(2)
print(last_error)
sys.exit(1)
PY
}

wait_console_worker() {
  "$PYTHON_BOOTSTRAP" - "$BASE_URL" "$API_TOKEN" <<'PY'
import json
import sys
import time
import urllib.request

base, token = sys.argv[1].rstrip("/"), sys.argv[2]
deadline = time.time() + 120
last = {}
while time.time() < deadline:
    req = urllib.request.Request(base + "/api/dashboard", headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            workers = data.get("workers") or []
            last = {"worker_count": len(workers), "workers": [w.get("worker_id") for w in workers if isinstance(w, dict)]}
            if workers:
                print(json.dumps(last, indent=2))
                sys.exit(0)
    except Exception as exc:
        last = {"error": str(exc)}
    time.sleep(2)
print(json.dumps(last, indent=2))
sys.exit(1)
PY
}

collect_compose_state() {
  if [ "$COMPOSE_AVAILABLE" -eq 1 ]; then
    compose ps > "$RESULT_DIR/compose_ps.txt" 2>&1 || true
    compose logs --no-color > "$RESULT_DIR/compose.log" 2>&1 || true
  fi
}

cleanup() {
  if [ "$COMPOSE_STARTED" -eq 1 ] && [ "$COMPOSE_AVAILABLE" -eq 1 ]; then
    collect_compose_state
    if ! bool_enabled "$KEEP_STACK"; then
      compose down --remove-orphans --volumes > "$RESULT_DIR/compose_down.log" 2>&1 || true
    fi
  fi
}
trap cleanup EXIT

package_results() {
  local tmp_package="$RESULT_DIR/../.$(basename "$RESULT_DIR").tmp.tar.gz"
  rm -f "$tmp_package" "$PACKAGE_PATH"
  tar \
    --exclude './console.env' \
    --exclude './console-acceptance-package.tar.gz' \
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

write_acceptance_summary() {
  "$PYTHON_BOOTSTRAP" scripts/summarize_console_acceptance.py "$RESULT_DIR" > "$RESULT_DIR/acceptance_summary.log" 2>&1
  local code=$?
  if [ "$code" -ne 0 ]; then
    SUMMARY_FAIL=1
  fi
  return 0
}

print_failure_context() {
  if [ "$ANY_FAIL" -eq 0 ] && [ "$SUMMARY_FAIL" -eq 0 ]; then
    return 0
  fi
  echo
  echo "==> failure_summary"
  if [ -f "$RESULT_DIR/acceptance_summary.md" ]; then
    cat "$RESULT_DIR/acceptance_summary.md"
  fi
}

write_server_environment
write_run_config
run_step prerequisite_check "$RESULT_DIR/prerequisite_check.log" check_prerequisites
if [ "$ANY_FAIL" -ne 0 ]; then
  PREREQ_OK=0
fi

if [ "$PREREQ_OK" -eq 1 ]; then
  run_step go_test_web "$RESULT_DIR/go_test_web.log" env -u LOGSERVE_API_TOKEN go test -count=1 ./cmd/logserve-web ./internal/webapi
  run_step go_vet_web "$RESULT_DIR/go_vet_web.log" go vet ./cmd/logserve-web ./internal/webapi
  if bool_enabled "$RUN_NPM_CI"; then
    run_step web_npm_ci "$RESULT_DIR/web_npm_ci.log" web_npm_ci
  fi
  run_step web_build "$RESULT_DIR/web_build.log" web_build
  run_step python_script_tests "$RESULT_DIR/python_script_tests.log" "$PYTHON_BOOTSTRAP" -m unittest discover tests/scripts

  if bool_enabled "$RUN_DOCKER"; then
    write_compose_env
    run_step docker_compose_config "$RESULT_DIR/docker_compose_config.log" compose_config_quiet
    if [ "$LAST_STEP_CODE" -eq 0 ]; then
      run_step docker_compose_build "$RESULT_DIR/docker_compose_build.log" compose build
    fi
    if [ "$LAST_STEP_CODE" -eq 0 ]; then
      run_step docker_compose_up "$RESULT_DIR/docker_compose_up.log" compose up -d postgres nats minio logd control web worker
      COMPOSE_STARTED=1
    fi
    if [ "$COMPOSE_STARTED" -eq 1 ]; then
      run_step web_health_ready "$RESULT_DIR/web_health_ready.log" wait_web_health
      run_step console_api_ready "$RESULT_DIR/console_api_ready.log" wait_console_api
      run_step console_worker_ready "$RESULT_DIR/console_worker_ready.log" wait_console_worker
      run_step console_http_probe "$RESULT_DIR/console_http_probe.log" "$PYTHON_BOOTSTRAP" scripts/console_http_probe.py --base-url "$BASE_URL" --token "$API_TOKEN" --timeout-sec "${LOGSERVE_CONSOLE_PROBE_TIMEOUT_SEC:-45}" --out "$RESULT_DIR/console_http_probe.json"
      collect_compose_state
    fi
  fi
fi

package_results
write_acceptance_summary
if [ "$SUMMARY_FAIL" -ne 0 ]; then
  ANY_FAIL=1
fi
print_failure_context

echo
echo "Ubuntu console acceptance directory: $RESULT_DIR"
echo "Summary: $RESULT_DIR/acceptance_summary.md"
echo "JSON: $RESULT_DIR/acceptance_summary.json"
echo "Package: $PACKAGE_PATH"
if bool_enabled "$KEEP_STACK" && [ "$COMPOSE_STARTED" -eq 1 ]; then
  echo "Compose stack kept for inspection: project=$COMPOSE_PROJECT base_url=$BASE_URL"
fi
exit "$ANY_FAIL"
