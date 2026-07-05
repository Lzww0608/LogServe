#!/usr/bin/env bash
set -euo pipefail

# Downloads a small pprof bundle from a running LogServe process. The target
# address, output directory, capture duration, and profile id can be overridden
# by arguments or LOGSERVE_PPROF_* environment variables.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"

ADDR="${1:-${LOGSERVE_PPROF_ADDR:-127.0.0.1:6062}}"
OUT_DIR="${2:-${LOGSERVE_PPROF_OUT:-benchmarks/profiles}}"
SECONDS="${LOGSERVE_PPROF_SECONDS:-30}"
mkdir -p "$OUT_DIR"
# Profile ids are guarded for the same reason benchmark ids are: stable names
# make report paths predictable and avoid timestamp-only artifact drift.
PROFILE_ID="${LOGSERVE_PPROF_ID:-latest}"
logserve_reject_dated_name "$PROFILE_ID" "LOGSERVE_PPROF_ID"
PREFIX="$OUT_DIR/profile-${PROFILE_ID}"
logserve_reject_dated_name "$PREFIX" "pprof output path"

BASE="http://${ADDR}/debug/pprof"

# Fetch CPU first because it blocks for SECONDS, then collect point-in-time
# heap, mutex, and block profiles from the same endpoint.
curl -fsS "${BASE}/profile?seconds=${SECONDS}" -o "${PREFIX}-cpu.pprof"
curl -fsS "${BASE}/heap" -o "${PREFIX}-heap.pprof"
curl -fsS "${BASE}/mutex" -o "${PREFIX}-mutex.pprof"
curl -fsS "${BASE}/block" -o "${PREFIX}-block.pprof"

# Write a manifest beside the binary profiles so downstream reports can refer
# to exact profile paths without guessing the naming convention.
cat >"${PREFIX}.json" <<EOF
{
  "addr": "${ADDR}",
  "seconds": ${SECONDS},
  "cpu": "${PREFIX}-cpu.pprof",
  "heap": "${PREFIX}-heap.pprof",
  "mutex": "${PREFIX}-mutex.pprof",
  "block": "${PREFIX}-block.pprof"
}
EOF

echo "Wrote profiles under ${PREFIX}-*.pprof"
