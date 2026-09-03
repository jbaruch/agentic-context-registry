# Installing acr

Release binaries support macOS and Linux on amd64 and arm64. Native Windows binaries remain outside the MVP in issue #14.

## Homebrew

Homebrew is the recommended installation path on both supported operating systems:

```shell
brew install jbaruch/acr/acr
```

Confirm the installed version:

```console
$ acr version
# fixture: bare
# exit: 0
<version>
```

The release workflow installs and tests the generated formula on macOS and Linux before it updates `jbaruch/homebrew-acr`.

## Direct download and verification

Choose the archive for the current machine:

| Operating system | Architecture | Asset |
| --- | --- | --- |
| macOS | Intel | `acr-darwin-amd64.tar.gz` |
| macOS | Apple silicon | `acr-darwin-arm64.tar.gz` |
| Linux | amd64 | `acr-linux-amd64.tar.gz` |
| Linux | arm64 | `acr-linux-arm64.tar.gz` |

Download one archive and the verification assets from the latest stable release. Replace the example asset when needed.

```shell
asset=acr-darwin-arm64.tar.gz
base=https://github.com/jbaruch/agentic-context-registry/releases/latest/download
curl -LO "$base/$asset"
curl -LO "$base/checksums.txt"
curl -LO "$base/checksums.txt.sigstore.json"
```

Verify the archive digest on macOS:

```shell
shasum -a 256 --ignore-missing -c checksums.txt
```

On Linux:

```shell
sha256sum --ignore-missing -c checksums.txt
```

The checksum manifest has one keyless Sigstore signature. Verify it with [cosign](https://docs.sigstore.dev/cosign/verifying/verify-blob/):

```shell
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/jbaruch/agentic-context-registry/\.github/workflows/release-cli\.yml@refs/tags/v' \
  checksums.txt
```

GitHub also stores keyless build provenance for every archive:

```shell
gh attestation verify "$asset" --repo jbaruch/agentic-context-registry
```

After all checks pass, extract the archive. It contains `acr` and its Apache-2.0 `LICENSE`.

```shell
tar -xzf "$asset"
```

```console
$ ./acr version --json
# fixture: bare
# exit: 0
{"ok":true,"command":"version","result":{"version":"<version>"}}
```

## Verifying on macOS

Apple notarization is deferred from the keyless MVP to [issue #39](https://github.com/jbaruch/agentic-context-registry/issues/39). `curl` does not set the `com.apple.quarantine` attribute, while browsers normally do. Go's linker ad-hoc-signs darwin/arm64 binaries; darwin/amd64 binaries are not Apple-signed.

The Gatekeeper consent sheet is operating-system behavior rather than an untested shipped module. Validate it manually on a Mac with Gatekeeper enabled:

1. Download the matching darwin archive in a browser, then extract it.
2. Run `./acr version`; expect Gatekeeper to prevent the quarantined, unnotarized binary from opening.
3. Run `xattr -d com.apple.quarantine ./acr`, or install the same release through Homebrew.
4. Run `./acr version --json`; expect no Gatekeeper sheet and values matching the release tag and source commit.

The manual check passes only when Gatekeeper blocks the quarantined browser download, then permits execution after the quarantine attribute is cleared, with the expected version and commit. Any other result fails the check. Checksums, the Sigstore bundle, GitHub provenance, cross-compilation, native execution, and Homebrew installation remain automated CI gates; no shipped module claims a testing carve-out.

## Go developer install

Developers with the Go version declared in `go.mod` can install an exact stable module version:

```shell
go install github.com/jbaruch/agentic-context-registry/cmd/acr@v1.2.3
```

Use `@latest` to request the latest stable module version. A Go proxy build reports the module version. It does not carry a VCS revision, so `acr version` omits the source commit for that installation path.

## Reproducing a release build

Use a clean checkout at the release tag and the Go version from `go.mod`. The clean Git state matters because Go embeds VCS metadata in the executable.

```shell
version=1.2.3
commit="$(git rev-parse HEAD)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath \
  -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
  -o acr ./cmd/acr
```

Change `GOOS` and `GOARCH` to one of the four supported pairs. Repeating the command with the same clean tagged source, toolchain, and flags produces identical bytes.
