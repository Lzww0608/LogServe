#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_CHECKPOINT_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_CHECKPOINT_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_CHECKPOINT_ACCEPTANCE_DIR:-"$ROOT/reports/checkpoint-acceptance-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_CHECKPOINT_ACCEPTANCE_DIR"
STATUS_FILE="$RESULT_DIR/command_status.jsonl"
ACCEPTANCE_JSON="$RESULT_DIR/checkpoint_acceptance.json"
PYTHON_RUN="${PYTHON:-python3}"
ANY_FAIL=0

mkdir -p "$RESULT_DIR"
: > "$STATUS_FILE"
cd "$ROOT" || exit 1

json_escape() {
  "$PYTHON_RUN" - "$1" <<'PY'
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

run_step checkpoint_unit_tests "$RESULT_DIR/checkpoint_unit_tests.log" env \
  -u LOGSERVE_API_TOKEN \
  -u LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT \
  go test -count=1 ./internal/control \
  -run 'Test(Bootstrap.*Checkpoint|MetadataCheckpoint.*|CreateMetadataCheckpoint.*|ControlRestartBootstrapsTaskAfterMetadataWriteLoss)'

run_step checkpoint_acceptance_go_test "$RESULT_DIR/checkpoint_acceptance_go_test.log" env \
  -u LOGSERVE_API_TOKEN \
  LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT="$ACCEPTANCE_JSON" \
  LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS:-120}" \
  LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS:-12}" \
  LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS:-12}" \
  LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS="${LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS:-40}" \
  go test -count=1 ./internal/control -run '^TestMetadataCheckpointAcceptanceReport$'

if [ "${LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_FULL_TESTS:-0}" = "1" ]; then
  run_step go_test_all "$RESULT_DIR/go_test_all.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT go test -count=1 ./...
fi

if [ "${LOGSERVE_CHECKPOINT_ACCEPTANCE_RUN_RACE:-0}" = "1" ]; then
  run_step go_race_control "$RESULT_DIR/go_race_control.log" env -u LOGSERVE_API_TOKEN -u LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT go test -race -count=1 ./internal/control
fi

run_step summarize_checkpoint_acceptance "$RESULT_DIR/summarize_checkpoint_acceptance.log" \
  "$PYTHON_RUN" scripts/summarize_checkpoint_acceptance.py "$RESULT_DIR"

if [ "$ANY_FAIL" -ne 0 ]; then
  echo
  echo "==> failure_summary"
  if [ -f "$RESULT_DIR/summary.md" ]; then
    cat "$RESULT_DIR/summary.md"
  elif [ -f "$RESULT_DIR/checkpoint_acceptance_go_test.log" ]; then
    tail -120 "$RESULT_DIR/checkpoint_acceptance_go_test.log" || true
  fi
fi

echo
echo "Checkpoint acceptance directory: $RESULT_DIR"
echo "Summary: $RESULT_DIR/summary.md"
echo "JSON: $RESULT_DIR/summary.json"
exit "$ANY_FAIL"
