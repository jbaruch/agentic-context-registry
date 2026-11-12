#!/usr/bin/env bash
# Ubuntu 24.04 ephemeral CI prerequisite, after the pinned bubblewrap install.
# Extract only Noble's experimental ABI4 profile; do not install the optional
# apparmor-profiles package (its scripts load unrelated policy).
# Review monthly and on Ubuntu AppArmor security updates: verify authenticated
# Noble package metadata, renew both hashes, and repeat native boundary proof.
main() {
  set -euo pipefail
  local version=4.0.1really4.0.1-0ubuntu0.24.04.7
  local archive_sha=bdac5b74d884643653565c52ed7483c9582e646ff72cce8d95d0eb8467a3139c
  local profile_sha=11d39094f044f0cda0febb3ad517b830301da6b2ce929664af09ee9e4dd264f9
  local target=/etc/apparmor.d/bwrap-userns-restrict
  local evidence="${RUNNER_TEMP:?}/bwrap-evidence" package profile resolved metadata entry before started
  mkdir -m 0700 "$evidence"
  exec > >(tee "$evidence/setup.log") 2>&1
  [[ "${RUNNER_OS:?}-${RUNNER_ARCH:?}" == Linux-X64 ]] || { echo 'Use an Ubuntu 24.04 amd64 runner.' >&2; exit 1; }
  [[ "$(id -u)" != 0 ]] || { echo 'Run boundary checks as the unprivileged runner, never root.' >&2; exit 1; }
  metadata="$(dpkg-query -W -f='${Version} ${Architecture} ${db:Status-Status}\n' bubblewrap)"
  [[ "$metadata" == '0.9.0-1ubuntu0.3 amd64 installed' ]] || { echo 'Install the pinned Noble bubblewrap package first.' >&2; exit 1; }
  resolved="$(readlink -f "$(command -v bwrap)")"
  [[ "$resolved" == /usr/bin/bwrap && "$(dpkg-query -S "$resolved")" == 'bubblewrap: /usr/bin/bwrap' ]] || { echo 'Refusing non-package bwrap or a mismatched profile attachment.' >&2; exit 1; }
  [[ "$(stat -c '%u:%g:%a' "$resolved")" == '0:0:755' ]] || { echo 'Refusing unexpected bwrap ownership, mode or setuid bits.' >&2; exit 1; }
  printf 'source=%s runner=%s/%s bubblewrap=%s path=%s\n' "$(git rev-parse HEAD)" "$RUNNER_OS" "$RUNNER_ARCH" "$metadata" "$resolved"
  sha256sum "$resolved"
  uname -sr
  dpkg-query -W -f='${Package} ${Version} ${Architecture}\n' apparmor
  apparmor_parser --version
  cat /proc/self/attr/current
  findmnt -n -o OPTIONS /proc
  for entry in /proc/sys/kernel/apparmor_restrict_unprivileged_userns /proc/sys/kernel/unprivileged_userns_clone /proc/sys/user/max_user_namespaces; do
    if [[ -f "$entry" ]]; then printf '%s=' "$entry"; cat "$entry"; else printf '%s absent\n' "$entry"; fi
  done
  sudo cat /sys/kernel/security/apparmor/profiles > "$evidence/profiles-before.txt"
  sudo cat /sys/kernel/security/apparmor/features/namespaces/mask
  sudo cat /sys/kernel/security/apparmor/features/domain/stack
  # Refuse local overrides, disabled/complain markers, existing named profiles,
  # or any other profile attaching to bwrap. Never overwrite operator policy.
  for entry in "$target" /etc/apparmor.d/local/bwrap-userns-restrict /etc/apparmor.d/local/unpriv_bwrap /etc/apparmor.d/force-complain/bwrap-userns-restrict /etc/apparmor.d/disable/bwrap-userns-restrict; do
    [[ ! -e "$entry" && ! -L "$entry" ]] || { printf 'Refusing existing profile state: %s\n' "$entry" >&2; exit 1; }
  done
  if grep -E '(^|//&)(bwrap|unpriv_bwrap)( |$|//)' "$evidence/profiles-before.txt"; then
    echo 'Refusing an existing bwrap profile; inspect host policy.' >&2; exit 1
  else
    [[ $? == 1 ]] || exit 1
  fi
  if sudo grep -R -n -E '(/usr/bin/bwrap|profile[[:space:]]+(bwrap|unpriv_bwrap)([[:space:]]|$))' /etc/apparmor.d; then
    echo 'Refusing competing bwrap attachment; inspect host policy.' >&2; exit 1
  else
    [[ $? == 1 ]] || exit 1
  fi
  started="$(date --iso-8601=seconds)"
  # Expected precondition failures are evidence, not passing negative canaries.
  if go test -count=1 -run '^TestCodexReadBoundaryDeniesInheritedInstructionsNatively$' -v ./internal/producerconvert > "$evidence/before.log" 2>&1; then before=0; else before=$?; fi
  printf '%s\n' "$before" > "$evidence/before.exit"
  cat "$evidence/before.log"
  # Only the kernel events for this single bwrap probe are retained.
  sudo journalctl -k --since "$started" --no-pager -o cat > "$evidence/kernel-window.log"
  if grep -E 'apparmor=.*(bwrap|unpriv_bwrap)|comm="bwrap"' "$evidence/kernel-window.log" > "$evidence/probe-audit.log"; then
    cat "$evidence/probe-audit.log"
  else
    [[ $? == 1 ]] || exit 1
    echo 'No matching bwrap audit events were available for the probe.'
  fi
  rm "$evidence/kernel-window.log"
  # apt-get uses the signature-verified metadata refreshed by the install step.
  # No allow-unauthenticated option or package maintainer script is used.
  (cd "$evidence" && apt-get download "apparmor-profiles=$version")
  package="$evidence/apparmor-profiles_${version}_all.deb"
  printf '%s  %s\n' "$archive_sha" "$package" | sha256sum -c -
  [[ "$(dpkg-deb -f "$package" Package Version Architecture)" == $'Package: apparmor-profiles\nVersion: '"$version"$'\nArchitecture: all' ]] || { echo 'Unexpected profile package metadata.' >&2; exit 1; }
  dpkg-deb -x "$package" "$evidence/extracted"
  profile="$evidence/extracted/usr/share/apparmor/extra-profiles/bwrap-userns-restrict"
  printf '%s  %s\n' "$profile_sha" "$profile" | sha256sum -c -
  [[ -f "$profile" && ! -L "$profile" ]] || { echo 'Expected a regular unchanged distro profile.' >&2; exit 1; }
  sudo install -o root -g root -m 0644 "$profile" "$target"
  # Compile against the actual kernel first; any parser warning fails closed.
  sudo apparmor_parser -Q -T "$target" > "$evidence/parser-check.log" 2>&1
  [[ ! -s "$evidence/parser-check.log" ]] || { cat "$evidence/parser-check.log"; echo 'Inspect parser warnings before enabling the profile.' >&2; exit 1; }
  sudo apparmor_parser -a -T "$target" > "$evidence/parser-load.log" 2>&1
  [[ ! -s "$evidence/parser-load.log" ]] || { cat "$evidence/parser-load.log"; echo 'Inspect parser load warnings.' >&2; exit 1; }
  sudo cat /sys/kernel/security/apparmor/profiles > "$evidence/profiles-after.txt"
  grep -Fx 'bwrap (enforce)' "$evidence/profiles-after.txt"
  grep -Fx 'unpriv_bwrap (enforce)' "$evidence/profiles-after.txt"
  sha256sum "$target"
  ACR_CODEX_EXPECT_APPARMOR=1 go test -count=1 -run '^TestCodexReadBoundaryDeniesInheritedInstructionsNatively$' -v ./internal/producerconvert 2>&1 | tee "$evidence/after.log"
  printf 'Scoped native boundary passed; before-profile exit=%s. This is not semantic conversion evidence.\n' "$before"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
