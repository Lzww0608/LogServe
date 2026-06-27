#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"

ADDR="${1:-${LOGSERVE_PPROF_ADDR:-127.0.0.1:6062}}"
OUT_DIR="${2:-${LOGSERVE_PPROF_OUT:-benchmarks/profiles}}"
SECONDS="${LOGSERVE_PPROF_SECONDS:-30}"
mkdir -p "$OUT_DIR"
PROFILE_ID="${LOGSERVE_PPROF_ID:-latest}"
logserve_reject_dated_name "$PROFILE_ID" "LOGSERVE_PPROF_ID"
PREFIX="$OUT_DIR/profile-${PROFILE_ID}"
logserve_reject_dated_name "$PREFIX" "pprof output path"

BASE="http://${ADDR}/debug/pprof"

curl -fsS "${BASE}/profile?seconds=${SECONDS}" -o "${PREFIX}-cpu.pprof"
curl -fsS "${BASE}/heap" -o "${PREFIX}-heap.pprof"
curl -fsS "${BASE}/mutex" -o "${PREFIX}-mutex.pprof"
curl -fsS "${BASE}/block" -o "${PREFIX}-block.pprof"

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
