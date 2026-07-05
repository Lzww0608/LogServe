#!/usr/bin/env bash
set -uo pipefail

# Thin wrapper for the frontend/admin/functions console acceptance path.
# It only chooses the result namespace, then delegates execution to the
# shared ubuntu_console_acceptance.sh harness.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
# The wrapper id is checked before use so generated reports do not drift
# into timestamped names that are hard to reference from documentation.
RUN_ID="${LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-console-frontend-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_DIR"
# Prefix the shared harness id so frontend-only reports remain distinct
# from full console and feature 6-10 acceptance runs.
INNER_ID="frontend-$RUN_ID"
logserve_reject_dated_name "$INNER_ID" "derived LOGSERVE_CONSOLE_ACCEPTANCE_ID"

# Respect a caller-provided inner id, otherwise bind the shared harness to
# this frontend-focused result directory.
export LOGSERVE_CONSOLE_ACCEPTANCE_ID="${LOGSERVE_CONSOLE_ACCEPTANCE_ID:-$INNER_ID}"
export LOGSERVE_CONSOLE_ACCEPTANCE_DIR="$RESULT_DIR"

echo "Running LogServe Console frontend/admin/functions acceptance"
echo "Result directory: $RESULT_DIR"
# Replace this wrapper process with the shared harness so callers receive its exact exit code.
exec bash "$ROOT/scripts/ubuntu_console_acceptance.sh"