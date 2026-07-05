#!/usr/bin/env bash
set -euo pipefail

# Runs the logstore benchmark command with environment-driven defaults and
# writes the JSON report to benchmarks/logstore_latest.json unless overridden.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${LOGSERVE_LOGBENCH_OUT:-"$ROOT/benchmarks/logstore_latest.json"}"

mkdir -p "$(dirname "$OUT")"
cd "$ROOT"

# The defaults keep local runs small enough for quick feedback while still
# exercising all fsync policies that logserve-logbench compares.
# Extra command-line arguments are passed through last so callers can add
# new benchmark flags without editing this wrapper.
go run ./cmd/logserve-logbench \
  --out "$OUT" \
  --records "${LOGSERVE_LOGBENCH_RECORDS:-20000}" \
  --streams "${LOGSERVE_LOGBENCH_STREAMS:-16}" \
  --payload-bytes "${LOGSERVE_LOGBENCH_PAYLOAD_BYTES:-256}" \
  --segment-size-bytes "${LOGSERVE_LOGBENCH_SEGMENT_SIZE_BYTES:-1048576}" \
  --policies "${LOGSERVE_LOGBENCH_POLICIES:-always,batch,interval}" \
  --fsync-interval-ms "${LOGSERVE_LOGBENCH_FSYNC_INTERVAL_MS:-100}" \
  "$@"

echo "wrote $OUT"
