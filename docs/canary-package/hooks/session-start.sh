#!/usr/bin/env bash
# Appends one marker line per session start so a human can tell whether the
# agent actually dispatched the hook. It reads nothing, reaches no network, and
# writes only to the canary log inside the scratch consumer.
set -euo pipefail

marker="ACR-CANARY-SESSION-START-7f3a"
log="${ACR_CANARY_LOG:-.acr-canary/session-start.log}"

directory="$(dirname -- "${log}")"
if ! mkdir -p -- "${directory}"; then
	printf '%s could not create %s\n' "${marker}" "${directory}" >&2
	exit 0
fi
if ! printf '%s %s\n' "${marker}" "${1:-session-start}" >>"${log}"; then
	printf '%s could not append to %s\n' "${marker}" "${log}" >&2
	exit 0
fi
