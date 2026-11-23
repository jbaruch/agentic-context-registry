#!/usr/bin/env bash
set -euo pipefail

main() {
  local probe_dir helper probe original line_count report status
  probe_dir=$(mktemp -d "$RUNNER_TEMP/pyright-probe.XXXXXX")
  helper=internal/adapter/acr-hook.py
  probe="$probe_dir/${helper##*/}"
  original=$(git hash-object --no-filters "$helper")
  cp "$helper" "$probe"
  pyright --project pyrightconfig.json --warnings "$probe"

  printf '\n_ci_pyright_probe: int = "deliberate type defect"\n' >> "$probe"
  line_count=$(wc -l < "$probe")
  if report=$(pyright --project pyrightconfig.json --warnings --outputjson "$probe"); then
    echo "Pyright accepted the deliberate type defect. Inspect the probe and Pyright configuration." >&2
    exit 1
  else
    status=$?
  fi
  printf '%s\n' "$report"
  if [[ "$status" != 1 ]]; then
    echo "Pyright probe exited $status. Inspect the engine and project configuration errors above." >&2
    exit "$status"
  fi
  if ! jq -e --arg file "$probe" --argjson line "$((line_count - 1))" '
    .generalDiagnostics | length == 1 and
    (.[0] | .file == $file and .range.start.line == $line and
      .severity == "error" and .rule == "reportAssignmentType")
  ' <<< "$report" > /dev/null; then
    echo "Pyright did not report the expected assignment error on the injected line. Inspect the probe diagnostics above." >&2
    exit 1
  fi
  if [[ "$(git hash-object --no-filters "$helper")" != "$original" ]]; then
    echo "The Pyright probe changed $helper. Inspect the probe copy and checkout." >&2
    exit 1
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main
fi
