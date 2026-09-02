#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd -P)"
acr="${ACR_BIN:-acr}"

emit_status() {
  local message="$1"
  local encoded="$message"
  encoded="${encoded//\\/\\\\}"
  encoded="${encoded//\"/\\\"}"
  encoded="${encoded//$'\b'/\\b}"
  encoded="${encoded//$'\f'/\\f}"
  encoded="${encoded//$'\n'/\\n}"
  encoded="${encoded//$'\r'/\\r}"
  encoded="${encoded//$'\t'/\\t}"

  if [[ -n "${CURSOR_VERSION:-}" ]]; then
    printf '{"additional_context":"%s"}\n' "$encoded"
    return
  fi
  printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"%s"}}\n' "$encoded"
}

if ! command -v "$acr" >/dev/null 2>&1; then
  emit_status "Session-start status — freshness: ACR could not find $acr; install acr or set ACR_BIN to its executable path."
  exit 0
fi

if output=$("$acr" freshness run --project "$root" "$@" 2>&1); then
  if [[ -n "$output" ]]; then
    emit_status "Session-start status — freshness:"$'\n'"$output"
  fi
else
  emit_status "Session-start status — freshness:"$'\n'"$output"$'\n'"ACR freshness check failed open; run \`acr freshness run --project $root $*\` to diagnose it."
fi

exit 0
