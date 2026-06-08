#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${LOGSERVE_LOGBENCH_OUT:-"$ROOT/benchmarks/logstore_v1_latest.json"}"

mkdir -p "$(dirname "$OUT")"
cd "$ROOT"

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
