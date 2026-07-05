#!/usr/bin/env bash
set -uo pipefail

# Thin wrapper for running only the console feature 6-10 acceptance path.
# It derives a stable sub-run id and delegates all real work to the shared
# ubuntu_console_acceptance.sh harness so the probe matrix stays centralized.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/scripts/naming_guard.sh"
# The public wrapper id is guarded before deriving the inner console
# acceptance id to keep both report directories canonical.
RUN_ID="${LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_ID:-latest}"
logserve_reject_dated_name "$RUN_ID" "LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_ID"
RESULT_DIR="${LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_DIR:-"$ROOT/reports/ubuntu-console-features-6-10-$RUN_ID"}"
logserve_reject_dated_name "$RESULT_DIR" "LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_DIR"
# Prefix the shared harness id so feature-only reports do not collide with
# full console acceptance runs that use the same outer RUN_ID.
INNER_ID="features-6-10-$RUN_ID"
logserve_reject_dated_name "$INNER_ID" "derived LOGSERVE_CONSOLE_ACCEPTANCE_ID"

# Respect an explicitly supplied inner id, otherwise route the shared
# harness into this wrapper-specific result namespace.
export LOGSERVE_CONSOLE_ACCEPTANCE_ID="${LOGSERVE_CONSOLE_ACCEPTANCE_ID:-$INNER_ID}"
export LOGSERVE_CONSOLE_ACCEPTANCE_DIR="$RESULT_DIR"

echo "Running LogServe Console feature 6-10 acceptance"
echo "Result directory: $RESULT_DIR"
# Replace this wrapper process with the shared harness so its exit code is preserved exactly.
exec bash "$ROOT/scripts/ubuntu_console_acceptance.sh"