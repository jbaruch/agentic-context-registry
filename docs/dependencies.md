# Dependency declarations and lockfile

ACR separates requested policy from immutable resolution. Project authors declare dependencies in `agents.yaml`; ACR writes concrete GitHub identities and verified package hashes to `.agents/registry.lock`.

## Project declarations

`agents.yaml` uses schema version 1. An omitted version on `acr install github:owner/plugin` and an explicit `@latest` both persist `requested: latest`:

```yaml
schemaVersion: 1
dependencies:
  - source: github:owner/plugin
    requested: latest
  - source: github:owner/pinned-plugin
    requested: v1.2.3
  - source: github:owner/commit-plugin
    requested: 0123456789abcdef0123456789abcdef01234567
```

Dependencies are sorted by canonical source. A 7–40 character hexadecimal request is a commit pin; every other valid fixed request is resolved as an exact GitHub Release tag. Because `@` separates the source from its version in CLI input, fixed tags containing `@` are not supported. Fixed tag and commit declarations remain pinned until the declaration changes. The machine-readable contract is [`schemas/agents.schema.json`](../schemas/agents.schema.json).

## Immutable lock

`.agents/registry.lock` uses schema version 1 and is written deterministically:

```yaml
schemaVersion: 1
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

The lockfile also carries the versioned target and entry ownership ledger used by the [transactional realization engine](realization.md). Dependency resolution preserves this ledger when it refreshes immutable locks.

## Resolution policy

- `latest` uses GitHub's latest stable Release endpoint, which excludes drafts and prereleases.
- Exact tags must name a non-draft, non-prerelease GitHub Release and must match `agent-plugin.yaml`'s version, allowing an optional leading `v`.
- Commit requests resolve once to a full 40-character commit and never query release metadata.
- `acr install` without a source refreshes `latest` declarations and reuses existing fixed locks.
- `acr outdated` resolves only the latest release/tag commit identities. It does not download archives or modify files.
- `acr update` refreshes eligible `latest` declarations; explicit pins remain fixed.

Downloaded GitHub tarballs are size-limited, extracted without materializing links or special files, validated through the package-manifest contract, and hashed before state is written. Invalid archives, package identity/version mismatches, and digest mismatches fail with recovery guidance.

## Authentication

Public repositories work without authentication. For private repositories and higher API limits, ACR checks `GH_TOKEN`, then `GITHUB_TOKEN`, then reuses `gh auth token`, and finally Git's configured HTTPS credential helper. Tokens are sent only to GitHub API requests and the allowlisted `https://codeload.github.com` archive origin; they are never written to project state or diagnostics.
