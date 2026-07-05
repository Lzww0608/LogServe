#!/usr/bin/env bash

# Shared naming guard for scripts that write benchmark or acceptance artifacts.
# It rejects empty names and timestamp-like ids so canonical report paths remain
# stable across repeated runs.

# logserve_reject_dated_name fails fast when a caller-provided artifact id or
# output path looks date-derived. Scripts source this helper before creating
# files so accidental timestamped report names are caught at the boundary.
logserve_reject_dated_name() {
  local value="${1:-}"
  local label="${2:-path name}"

  if [ -z "$value" ]; then
    echo "$label must not be empty" >&2
    exit 1
  fi

  if [[ "$value" =~ (20[0-9]{6}|20[0-9]{2}-[0-9]{2}-[0-9]{2}|[0-9]{8}T[0-9]+Z|exp[0-9]{6,}) ]]; then
    # Match common compact dates, ISO-like dates, UTC timestamp ids, and legacy
    # expNNNNNN names while leaving ordinary semantic ids untouched.
    echo "$label must not contain dates or timestamp-like ids: $value" >&2
    exit 1
  fi
}
