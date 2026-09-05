#!/usr/bin/env bash
# Appends one marker line per session start so a human can tell whether the
# agent actually dispatched the hook. It reads nothing, reaches no network, and
# writes only to the canary log inside the scratch consumer.
set -euo pipefail

marker="ACR-CANARY-SESSION-START-7f3a"

# append_marker records one dispatch in the canary log, naming the event it was
# given. A log it cannot write is reported on stderr and never faked; the
# session the agent is starting continues either way.
append_marker() {
	local event log directory
	event="${1:-session-start}"
	log="${ACR_CANARY_LOG:-.acr-canary/session-start.log}"
	directory="$(dirname -- "${log}")"
	if ! mkdir -p -- "${directory}"; then
		printf '%s could not create %s\n' "${marker}" "${directory}" >&2
		return 0
	fi
	if ! printf '%s %s\n' "${marker}" "${event}" >>"${log}"; then
		printf '%s could not append to %s\n' "${marker}" "${log}" >&2
		return 0
	fi
}

# Sourcing this file defines append_marker and writes nothing, so a test can
# import it without a dispatch being recorded.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	append_marker "${1:-session-start}"
fi
