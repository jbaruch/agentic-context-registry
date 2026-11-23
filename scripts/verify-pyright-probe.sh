#!/usr/bin/env bash
set -euo pipefail

cleanup() {
  if ! rm -rf -- "$1"; then
    printf 'Could not remove Pyright probe directory %s. Check directory permissions.\n' "$1" >&2
  fi
  return 0
}

main() {
  local helper probe original line_count report status
  helper=internal/adapter/acr-hook.py
  probe="$1/${helper##*/}"
  original=$(git hash-object --no-filters "$helper")
  cp "$helper" "$probe"
  if report=$(pyright --project pyrightconfig.json --warnings "$probe" 2>&1); then
    :
  else
    status=$?
    printf '%s\n' "$report" >&2
    printf 'Pyright clean control exited %s. Inspect the clean probe copy diagnostics and pyrightconfig.json, then rerun the probe.\n' "$status" >&2
    exit "$status"
  fi

  printf '\n_ci_pyright_probe: int = "deliberate type defect"\n' >> "$probe"
  line_count=$(wc -l < "$probe")
  if report=$(pyright --project pyrightconfig.json --warnings --outputjson "$probe"); then
    echo "Pyright accepted the deliberate type defect. Inspect the probe and Pyright configuration." >&2
    exit 1
  else
    status=$?
  fi
  if [[ "$status" != 1 ]]; then
    printf '%s\n' "$report" >&2
    echo "Pyright probe exited $status. Inspect the engine and project configuration errors above." >&2
    exit "$status"
  fi
  if ! jq -e --arg file "$probe" --argjson line "$((line_count - 1))" '
    .generalDiagnostics | length == 1 and
    (.[0] | .file == $file and .range.start.line == $line and
      .severity == "error" and .rule == "reportAssignmentType")
  ' <<< "$report" > /dev/null; then
    printf '%s\n' "$report" >&2
    echo "Pyright did not report the expected assignment error on the injected line. Inspect the probe diagnostics above." >&2
    exit 1
  fi
  if [[ "$(git hash-object --no-filters "$helper")" != "$original" ]]; then
    echo "The Pyright probe changed $helper. Inspect the probe copy and checkout." >&2
    exit 1
  fi
  printf '%s\n' "$report"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  probe_dir=$(mktemp -d "$RUNNER_TEMP/pyright-probe.XXXXXX")
  trap 'cleanup "$probe_dir"' EXIT
  main "$probe_dir"
fi
