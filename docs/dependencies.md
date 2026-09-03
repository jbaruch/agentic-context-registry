# Dependency declarations and lockfile

ACR separates requested policy from immutable resolution. Project authors declare dependencies in `agents.yaml`; ACR writes concrete GitHub or local vendor identities and verified package hashes to `.agents/registry.lock`.

## Project declarations

`agents.yaml` uses schema version 2. An omitted version on `acr install github:owner/plugin` and an explicit `@latest` both persist `requested: latest`:

```yaml
schemaVersion: 2
dependencies:
  - source: github:owner/plugin
    requested: latest
  - source: github:owner/pinned-plugin
    requested: v1.2.3
  - source: github:owner/commit-plugin
    requested: 0123456789abcdef0123456789abcdef01234567
```

Dependencies are sorted by canonical source. A 7–40 character hexadecimal request is a commit pin; every other valid fixed request is resolved as an exact GitHub Release tag. Because `@` separates the source from its version in CLI input, fixed tags containing `@` are not supported. Fixed tag and commit declarations remain pinned until the declaration changes. The machine-readable contract is [`schemas/agents.schema.json`](../schemas/agents.schema.json).

Tessl migration can add a local declaration under schema version 3:

```yaml
schemaVersion: 3
dependencies:
  - source: vendor:workspace/package
    requested: vendored
```

`vendored` is a scheme-bound constant, not a version or commit-like reference. Vendor declarations cannot carry rollback holds.

## Immutable lock

`.agents/registry.lock` uses the minimum schema its contents require: version 2 for GitHub-only state and version 3 when it records a local vendor resolution. It is written deterministically:

```yaml
schemaVersion: 2
dependencies:
  - source: github:owner/plugin
    requested: latest
    kind: release
    releaseId: 123456
    tag: v2.0.0
    commit: 0123456789abcdef0123456789abcdef01234567
    packageVersion: 2.0.0
    contentHash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Release locks record the GitHub release database ID and exact tag. Every lock records the full commit, the validated package-manifest version, and a SHA-256 digest over the sorted published package files, including their paths, modes, sizes, and file digests. Commit locks omit `releaseId` and `tag`. See [`schemas/registry-lock.schema.json`](../schemas/registry-lock.schema.json).

Realization consumes the full locked commit and verifies the downloaded content hash; it does not resolve tags or releases again. A hash mismatch is a hard failure.

A vendor lock records the local identity, original Tessl version, and all-file hash without GitHub metadata:

```yaml
schemaVersion: 3
dependencies:
  - source: vendor:workspace/package
    requested: vendored
    kind: vendor
    packageVersion: legacy-version
    contentHash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

The package lives at `.agents/vendor/workspace/package`. Realization never downloads it or deletes it during cleanup; it recomputes the content hash and fails if local bytes or normalized executable modes differ.

The lockfile also carries the versioned target and entry ownership ledger used by the [transactional realization engine](realization.md). Dependency resolution preserves this ledger when it refreshes immutable locks.

## Rollback holds

A dependency whose `latest` resolution breaks the project can be rolled back without giving up `latest`. The declaration keeps `requested: latest` and gains a `hold` recording the known-good pin and the rejected release:

```yaml
schemaVersion: 2
dependencies:
  - source: github:owner/plugin
    requested: latest
    hold:
      pin: v1.3.2
      rejected: v1.4.0
      reason: 1.4.0 breaks the review hook
```

`hold.pin` is a release tag or commit SHA; `hold.rejected` is always a release tag, because a hold can only be created from a `latest` resolution and `latest` only ever resolves to a stable Release. `reason` is optional and free text. A hold is legal only on `requested: latest`, which is the invariant that keeps it from ever presenting itself as a permanent pin. Holds carry no timestamp: a clock-derived value in a committed file would make `acr install` non-deterministic, and `git blame` already dates the hold.

The lock records the held release as an ordinary lock row plus the barrier's resolved identity, which `agents.yaml` cannot carry because it holds tag names only:

```yaml
  - source: github:owner/plugin
    requested: latest
    kind: release
    releaseId: 987
    tag: v1.3.2
    commit: 0123456789abcdef0123456789abcdef01234567
    packageVersion: 1.3.2
    contentHash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    hold:
      rejectedTag: v1.4.0
      rejectedReleaseId: 1024
      rejectedCommit: fedcba9876543210fedcba9876543210fedcba98
```

When a hold is present, the release/commit consistency rules key off `hold.pin` rather than the request, which is what lets a held commit pin coexist with `requested: latest`.

A rollback differs from a permanent pin. A permanent pin is `requested: v1.3.2` with no hold: it never consults release metadata, never appears in `acr outdated`, and has no barrier or resume path. A hold keeps the dependency inside the `latest` freshness surface and carries an explicit, reviewable exit.

### Barrier semantics

- A candidate whose tag equals `hold.rejected` preserves the held pin silently, whatever its release ID or commit. Tag equality is tested before any ordering, so retagging and republishing can never clear a barrier.
- A stable candidate strictly newer than the barrier still preserves the held pin, and adds a `dependency_hold_resumable` notice naming `acr resume`. Nothing resumes automatically.
- Drafts and prereleases never clear a barrier. `latest` already excludes them, and the policy rejects them again.
- Ordering prefers semver on both tags. For non-semver tags it falls back to GitHub's creation-ordered release IDs when the barrier's ID is recorded, and otherwise the barrier stands. The comparison gates only the notice, never locked state.
- A second rollback advances the barrier to the release being rejected now only where that release is proven newer than the standing barrier: by semver, or by the release identities the lock already records. An unprovable pair keeps the standing barrier, so deepening a rollback never re-exposes a release an earlier rollback rejected.
- While a hold stands, `--hold` and `--pin` accept only a reference proven not to move the held resolution forward: the reference the lock already resolves, or a semver-older tag. A newer or unorderable reference is refused with guidance to review the barrier and run `acr resume`, so no flag is an alternative route past it.

Removing a hold is always explicit: `acr resume SOURCE` retires the barrier, and `acr install SOURCE@REF --pin` converts the hold into a permanent pin. See the [CLI reference](cli.md#rollback-holds) for the command surface.

## Schema versions

Both files moved from schema version 1 to 2 when holds were introduced. Version 3 adds `vendor:` sources and `kind: vendor`. Readers accept versions 1 through 3, while each file is stamped with the minimum version its own content requires: vendor-free state remains version 2 and a file containing vendor state is version 3. Read-only commands leave on-disk versions untouched.

An `acr` predating holds refuses a version 2 file with an `unsupported schemaVersion` error rather than ignoring an unrecognized `hold` field and reinstalling the rejected release. A file that records a hold while still stamped version 1 is refused by both the runtime and the JSON Schemas: that stamp reads as understood to an older `acr`, which would then resolve `latest` straight over the barrier. Stamp `schemaVersion: 2` on such a file. `internal/dependency` owns both files and is their sole migrator.

The same rule applies to vendor state: a `vendor:` declaration or lock under schema version 1 or 2 is refused, and a future version above 3 tells the operator to upgrade ACR rather than downgrade a correct file.

## Resolution policy

- `latest` uses GitHub's latest stable Release endpoint, which excludes drafts and prereleases.
- Exact tags must name a non-draft, non-prerelease GitHub Release and must match `agent-plugin.yaml`'s version, allowing an optional leading `v`.
- Commit requests resolve once to a full 40-character commit and never query release metadata.
- `acr install` without a source refreshes `latest` declarations and reuses existing fixed locks.
- `acr outdated` resolves only the latest release/tag commit identities. It does not download archives or modify files.
- `acr update` refreshes eligible `latest` declarations; explicit pins remain fixed.
- `acr install`, `acr update`, and the session-start `install` policy all consult one hold policy, so none of them can reinstall a rejected release.
- `acr resume SOURCE` is the only command that resumes `latest` for a held dependency. `acr install SOURCE@REF --pin` is the only other command that ends a hold, and it replaces `latest` rather than resuming it.
- Vendored dependencies are local and non-actionable in `acr outdated`; direct install, targeted update, and resume commands refuse them with migration guidance.

Downloaded GitHub tarballs are size-limited, extracted without materializing links or special files, validated through the package-manifest contract, and hashed before state is written. Invalid archives, package identity/version mismatches, and digest mismatches fail with recovery guidance.

An ACR-published release includes `acr-package.json`. When that asset is present and uses a supported metadata version, resolution verifies its recorded commit and content hash against the tag-resolved source tree. A mismatch is a hard failure that detects a moved tag or inconsistent release evidence. Releases without the asset, unsupported future metadata versions, and temporarily unavailable metadata retain the source-tree installation path for compatibility. Commit-pinned installation never queries release metadata.

## Authentication

Public repositories work without authentication. For private repositories and higher API limits, ACR checks `GH_TOKEN`, then `GITHUB_TOKEN`, then reuses `gh auth token`, and finally Git's configured HTTPS credential helper. Tokens are sent only to GitHub API requests and the allowlisted `https://codeload.github.com` archive origin; they are never written to project state or diagnostics. Release asset redirects are restricted to `https://objects.githubusercontent.com` and `https://release-assets.githubusercontent.com` and carry no bearer token.
