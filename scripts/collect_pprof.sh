#!/usr/bin/env bash
set -euo pipefail

ADDR="${1:-${LOGSERVE_PPROF_ADDR:-127.0.0.1:6062}}"
OUT_DIR="${2:-${LOGSERVE_PPROF_OUT:-benchmarks/profiles}}"
SECONDS="${LOGSERVE_PPROF_SECONDS:-30}"
mkdir -p "$OUT_DIR"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
PREFIX="$OUT_DIR/profile-${STAMP}"

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
