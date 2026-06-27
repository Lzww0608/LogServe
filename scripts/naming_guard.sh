#!/usr/bin/env bash

logserve_reject_dated_name() {
  local value="${1:-}"
  local label="${2:-path name}"

  if [ -z "$value" ]; then
    echo "$label must not be empty" >&2
    exit 1
  fi

  if [[ "$value" =~ (20[0-9]{6}|20[0-9]{2}-[0-9]{2}-[0-9]{2}|[0-9]{8}T[0-9]+Z|exp[0-9]{6,}) ]]; then
    echo "$label must not contain dates or timestamp-like ids: $value" >&2
    exit 1
  fi
}
