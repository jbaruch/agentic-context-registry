#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd -P)"
acr="${ACR_BIN:-acr}"

if ! command -v "$acr" >/dev/null 2>&1; then
  printf 'ACR freshness check could not find %s; install acr or set ACR_BIN to its executable path.\n' "$acr" >&2
  exit 0
fi

if ! "$acr" freshness run --project "$root" "$@"; then
  printf 'ACR freshness check failed open; run `acr freshness run --project %s %s` to diagnose it.\n' "$root" "$*" >&2
fi

exit 0
