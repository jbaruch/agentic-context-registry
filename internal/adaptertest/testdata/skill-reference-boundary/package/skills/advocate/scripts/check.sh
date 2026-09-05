#!/usr/bin/env bash
set -euo pipefail

# Output contract: one JSON object on stdout.
printf '%s\n' "{\"ok\":true,\"helper\":\"advocate-check\",\"argument\":\"${1:-none}\"}"
