#!/usr/bin/env bash
set -euo pipefail

readonly SOURCE_REPOSITORY="jbaruch/agentic-context-registry"
readonly TAP_REPOSITORY="jbaruch/homebrew-acr"
readonly TAP_SSH_URL="git@github.com:jbaruch/homebrew-acr.git"
readonly DEPLOY_KEY_TITLE="acr-release-formula"
readonly SECRET_NAME="HOMEBREW_TAP_DEPLOY_KEY"

temporary_directory=""
staged_key_id=""

usage() {
  printf 'Usage: %s [--rotate]\n' "${0##*/}"
  printf 'Without --rotate, complete provisioning is unchanged and a deploy key without %s is repaired by rotation.\n' \
    "${SECRET_NAME}"
}

cleanup() {
  if [[ -n "${staged_key_id}" ]]; then
    printf 'Removing staged deploy key %s after provisioning was interrupted.\n' "${staged_key_id}" >&2
    if delete_deploy_key "${staged_key_id}"; then
      staged_key_id=""
    fi
  fi
  if [[ -n "${temporary_directory:-}" && -d "${temporary_directory:-}" ]]; then
    rm -rf -- "${temporary_directory}"
  fi
  return 0
}

handle_signal() {
  local signal="$1"
  cleanup
  trap - "${signal}" EXIT
  kill -s "${signal}" "$$"
  return 0
}

require_command() {
  local command_name="$1"
  if ! command -v "${command_name}" >/dev/null; then
    printf 'Required command %s is unavailable; install it and retry.\n' "${command_name}" >&2
    return 1
  fi
}

delete_deploy_key() {
  local key_id="$1"
  if ! gh api --method DELETE "repos/${TAP_REPOSITORY}/keys/${key_id}" >/dev/null; then
    printf 'Could not remove deploy key %s from %s; inspect the repository deploy keys before retrying.\n' \
      "${key_id}" "${TAP_REPOSITORY}" >&2
    return 1
  fi
}

rollback_staged_key() {
  local key_id="$1"
  printf 'Removing staged deploy key %s after provisioning failed.\n' "${key_id}" >&2
  if delete_deploy_key "${key_id}"; then
    staged_key_id=""
    return 0
  fi
  return 1
}

main() {
  local rotate=false
  case "$#" in
    0) ;;
    1)
      case "$1" in
        --rotate) rotate=true ;;
        -h | --help)
          usage
          return 0
          ;;
        *)
          usage >&2
          printf 'Unknown argument %s; pass --rotate or no argument.\n' "$1" >&2
          return 2
          ;;
      esac
      ;;
    *)
      usage >&2
      printf 'Too many arguments; pass --rotate or no argument.\n' >&2
      return 2
      ;;
  esac

  local command_name
  for command_name in gh git mktemp ssh ssh-keygen; do
    require_command "${command_name}"
  done

  if ! gh auth status --hostname github.com >/dev/null; then
    printf 'GitHub CLI is not authenticated; run gh auth login --hostname github.com and retry.\n' >&2
    return 1
  fi

  local existing_output
  if ! existing_output="$(
    gh api "repos/${TAP_REPOSITORY}/keys" --paginate \
      --jq ".[] | select(.title == \"${DEPLOY_KEY_TITLE}\") | .id"
  )"; then
    printf 'Could not list deploy keys for %s; confirm your GitHub access and retry.\n' "${TAP_REPOSITORY}" >&2
    return 1
  fi

  local -a existing_key_ids=()
  local existing_key_id
  while IFS= read -r existing_key_id; do
    if [[ -z "${existing_key_id}" ]]; then
      continue
    fi
    if [[ ! "${existing_key_id}" =~ ^[0-9]+$ ]]; then
      printf 'GitHub returned invalid deploy key id %s; inspect %s deploy keys and retry.\n' \
        "${existing_key_id}" "${TAP_REPOSITORY}" >&2
      return 1
    fi
    existing_key_ids+=("${existing_key_id}")
  done <<< "${existing_output}"

  local secret_output
  if ! secret_output="$(
    gh secret list --repo "${SOURCE_REPOSITORY}" --json name \
      --jq ".[] | select(.name == \"${SECRET_NAME}\") | .name"
  )"; then
    printf 'Could not check %s for %s; confirm Actions secret administration access and retry.\n' \
      "${SECRET_NAME}" "${SOURCE_REPOSITORY}" >&2
    return 1
  fi
  local secret_present=false
  if [[ "${secret_output}" == "${SECRET_NAME}" ]]; then
    secret_present=true
  fi

  if [[ "${#existing_key_ids[@]}" -gt 0 && "${secret_present}" == true && "${rotate}" == false ]]; then
    printf '{"action":"unchanged","deployKeyTitle":"%s","secret":"%s","sourceRepository":"%s","tapRepository":"%s","undeletedDeployKeyIds":[]}\n' \
      "${DEPLOY_KEY_TITLE}" "${SECRET_NAME}" "${SOURCE_REPOSITORY}" "${TAP_REPOSITORY}"
    return 0
  fi

  local repair_incomplete=false
  if [[ "${#existing_key_ids[@]}" -gt 0 && "${secret_present}" == false && "${rotate}" == false ]]; then
    repair_incomplete=true
    printf 'Provisioning is incomplete: %s has the deploy key but %s lacks %s; rotating the unrecoverable key.\n' \
      "${TAP_REPOSITORY}" "${SOURCE_REPOSITORY}" "${SECRET_NAME}" >&2
  fi

  trap cleanup EXIT
  trap 'handle_signal INT' INT
  trap 'handle_signal TERM' TERM
  trap 'handle_signal HUP' HUP
  temporary_directory="$(mktemp -d)"
  local private_key="${temporary_directory}/deploy-key"
  local public_key="${private_key}.pub"
  local known_hosts="${temporary_directory}/known-hosts"

  printf 'Fetching GitHub published SSH host keys.\n' >&2
  if ! gh api https://api.github.com/meta \
    --jq '.ssh_keys[] | "github.com " + .' > "${known_hosts}"; then
    printf 'Could not fetch GitHub SSH host keys over TLS; confirm access to https://api.github.com/meta and retry.\n' >&2
    return 1
  fi
  if [[ ! -s "${known_hosts}" ]]; then
    printf 'GitHub metadata returned no SSH host keys; retry after confirming https://api.github.com/meta lists ssh_keys.\n' >&2
    return 1
  fi

  printf 'Generating a new deploy key for %s.\n' "${TAP_REPOSITORY}" >&2
  if ! ssh-keygen -q -t ed25519 -N '' -C "${DEPLOY_KEY_TITLE}" -f "${private_key}"; then
    printf 'Could not generate an ed25519 key pair; confirm ssh-keygen works and retry.\n' >&2
    return 1
  fi

  local public_key_material
  public_key_material="$(<"${public_key}")"
  local new_key_id
  if ! new_key_id="$(
    gh api --method POST "repos/${TAP_REPOSITORY}/keys" \
      --raw-field title="${DEPLOY_KEY_TITLE}" \
      --raw-field key="${public_key_material}" \
      --field read_only=false \
      --jq '.id'
  )"; then
    printf 'Could not register the write deploy key on %s; confirm deploy-key administration access and retry.\n' \
      "${TAP_REPOSITORY}" >&2
    return 1
  fi
  if [[ ! "${new_key_id}" =~ ^[0-9]+$ ]]; then
    printf 'GitHub returned invalid new deploy key id %s; inspect %s deploy keys before retrying.\n' \
      "${new_key_id}" "${TAP_REPOSITORY}" >&2
    return 1
  fi
  staged_key_id="${new_key_id}"

  printf 'Verifying the staged deploy key against %s.\n' "${TAP_REPOSITORY}" >&2
  if ! GIT_SSH_COMMAND="ssh -i ${private_key} -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=${known_hosts}" \
    git ls-remote "${TAP_SSH_URL}" > "${temporary_directory}/ls-remote.out"; then
    printf 'The staged deploy key could not authenticate to %s; removing it before retry.\n' \
      "${TAP_REPOSITORY}" >&2
    rollback_staged_key "${new_key_id}"
    return 1
  fi

  printf 'Storing %s for %s.\n' "${SECRET_NAME}" "${SOURCE_REPOSITORY}" >&2
  if ! gh secret set "${SECRET_NAME}" --repo "${SOURCE_REPOSITORY}" < "${private_key}"; then
    printf 'Could not store %s; preserving the previous credential and removing the staged deploy key.\n' \
      "${SECRET_NAME}" >&2
    rollback_staged_key "${new_key_id}"
    return 1
  fi
  staged_key_id=""

  local -a undeleted_key_ids=()
  if [[ "${#existing_key_ids[@]}" -gt 0 ]]; then
    for existing_key_id in "${existing_key_ids[@]}"; do
      printf 'Removing superseded deploy key %s from %s.\n' "${existing_key_id}" "${TAP_REPOSITORY}" >&2
      if ! delete_deploy_key "${existing_key_id}"; then
        undeleted_key_ids+=("${existing_key_id}")
      fi
    done
  fi
  if [[ "${#undeleted_key_ids[@]}" -gt 0 ]]; then
    printf 'Warning: the new credential is active, but superseded deploy key IDs %s could not be removed; delete them from %s.\n' \
      "$(IFS=,; printf '%s' "${undeleted_key_ids[*]}")" "${TAP_REPOSITORY}" >&2
  fi

  local action="created"
  if [[ "${repair_incomplete}" == true ]]; then
    action="repaired"
  elif [[ "${rotate}" == true ]]; then
    action="rotated"
  fi
  local undeleted_key_ids_json="[]"
  if [[ "${#undeleted_key_ids[@]}" -gt 0 ]]; then
    undeleted_key_ids_json="[$(IFS=,; printf '%s' "${undeleted_key_ids[*]}")]"
  fi
  printf '{"action":"%s","deployKeyTitle":"%s","secret":"%s","sourceRepository":"%s","tapRepository":"%s","undeletedDeployKeyIds":%s}\n' \
    "${action}" "${DEPLOY_KEY_TITLE}" "${SECRET_NAME}" "${SOURCE_REPOSITORY}" "${TAP_REPOSITORY}" \
    "${undeleted_key_ids_json}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
