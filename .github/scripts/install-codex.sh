#!/usr/bin/env bash
# Install one official Codex CLI release for the runner's platform from the
# GitHub release asset, verify its published sha256, and export
# ACR_CODEX_RELEASE_BIN for the steps that follow.
#
# Usage: install-codex.sh VERSION
#   RUNNER_OS, RUNNER_ARCH, RUNNER_TEMP and GITHUB_ENV come from the runner.
#
# The digest table is the pin. Renew it when internal/producerconvert's
# CodexVerifiedReleases gains a release: copy the sha256 GitHub reports for
# the asset (`gh release view rust-v<version> --repo openai/codex --json
# assets`) and add the four rows. Review monthly beside the GitHub Actions
# pins. An unknown version or platform fails closed here, before anything
# is downloaded.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: install-codex.sh VERSION" >&2
  exit 2
fi
version=$1

case "${RUNNER_OS}-${RUNNER_ARCH}" in
  Linux-X64) asset=codex-x86_64-unknown-linux-musl ;;
  Linux-ARM64) asset=codex-aarch64-unknown-linux-musl ;;
  macOS-ARM64) asset=codex-aarch64-apple-darwin ;;
  macOS-X64) asset=codex-x86_64-apple-darwin ;;
  *)
    echo "no official Codex native asset for runner ${RUNNER_OS}-${RUNNER_ARCH}; add it to install-codex.sh" >&2
    exit 1
    ;;
esac

case "${version}/${asset}" in
  0.153.2/codex-x86_64-unknown-linux-musl) sha256=e8cd1160071f725d2a10cab81073dd6818fc8b096372125d27ef6e66fdf0979e ;;
  0.153.2/codex-aarch64-unknown-linux-musl) sha256=878693f9b370320ea21793f99ea1f5687b7d9aa1f2c733de693d9ec0baa4e62a ;;
  0.153.2/codex-aarch64-apple-darwin) sha256=91dfc270f0dfbaec16d814f1aa90d4f27e74dc9e3784e64006bef3b79fe9e09c ;;
  0.153.2/codex-x86_64-apple-darwin) sha256=d84515df27b14255a1c4fe28827ba5975a095a514fe6755330e8dee5cc21ee7a ;;
  0.154.0/codex-x86_64-unknown-linux-musl) sha256=d7e18b2597ae8f242f5f31ee9e90deef48dbc9edd634d9868fb6435d08c07f02 ;;
  0.154.0/codex-aarch64-unknown-linux-musl) sha256=583b48df32804213bdcd338c2e5adb06b34340821fa757a726cc0a524fa33c27 ;;
  0.154.0/codex-aarch64-apple-darwin) sha256=344310a0a591c1b192e04feff304321a69907c9498baaac331ca7e16ebcef9d7 ;;
  0.154.0/codex-x86_64-apple-darwin) sha256=1219c837d8f813b493a424c125c0038b5d9ca16279bc6d3fe6ce037a3e18a6e7 ;;
  0.155.0/codex-x86_64-unknown-linux-musl) sha256=e415cc3adb94ade16e8d44b4dd58a9201cc34b2ee51a5d6eddf2a3a00aecb6c0 ;;
  0.155.0/codex-aarch64-unknown-linux-musl) sha256=8b4a9c356916c515f7c93f918a01b8fa1371bcc9758addbfa723b85fbec5694b ;;
  0.155.0/codex-aarch64-apple-darwin) sha256=5a584b7cddc2a97083cada53f10f5bc4231526b7f64105f6a5bb82d01ccdba49 ;;
  0.155.0/codex-x86_64-apple-darwin) sha256=cc84081b15284eea10c8b8d428818d1c8debdce5a7f0f4f5c04dfc7c72518174 ;;
  *)
    echo "Codex ${version} for ${asset} is not in the verified release table; add its published sha256 to install-codex.sh after verifying the release" >&2
    exit 1
    ;;
esac

destination="${RUNNER_TEMP}/codex-${version}"
archive="${destination}.tar.gz"
mkdir -p "${destination}"
curl -fsSL --retry 3 -o "${archive}" "https://github.com/openai/codex/releases/download/rust-v${version}/${asset}.tar.gz"
printf '%s  %s\n' "${sha256}" "${archive}" | shasum -a 256 -c -
tar -xzf "${archive}" -C "${destination}"
binary="${destination}/${asset}"
if [[ ! -f "${binary}" ]]; then
  echo "release archive did not contain ${asset}" >&2
  exit 1
fi
chmod 0755 "${binary}"
printf 'ACR_CODEX_RELEASE_BIN=%s\n' "${binary}" >> "${GITHUB_ENV}"
printf 'ACR_CODEX_RELEASE_VERSION=%s\n' "${version}" >> "${GITHUB_ENV}"
# A `codex` on PATH lets the live lane resolve the executable the way an
# operator's shell does.
mkdir -p "${destination}/bin"
ln -sf "${binary}" "${destination}/bin/codex"
printf '%s\n' "${destination}/bin" >> "${GITHUB_PATH}"
echo "installed codex ${version} (${asset}) at ${binary}"
