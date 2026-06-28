#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
RUN_ID="${LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-console-frontend-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_DIR"
INNER_ID="frontend-$RUN_ID"
logserve_reject_dated_name "$INNER_ID" "derived LOGSERVE_CONSOLE_ACCEPTANCE_ID"

export LOGSERVE_CONSOLE_ACCEPTANCE_ID="${LOGSERVE_CONSOLE_ACCEPTANCE_ID:-$INNER_ID}"
export LOGSERVE_CONSOLE_ACCEPTANCE_DIR="$RESULT_DIR"

echo "Running LogServe Console frontend/admin/functions acceptance"
echo "Result directory: $RESULT_DIR"
exec bash "$ROOT/scripts/ubuntu_console_acceptance.sh"