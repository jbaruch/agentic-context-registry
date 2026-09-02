# Publishing packages

ACR publishes immutable package evidence as GitHub Release assets. The tagged Git source tree remains the installable package: `acr install github:owner/repository[@TAG|@SHA]` downloads the repository archive at the resolved commit and does not require a hosted registry or discovery service.

## Publish a package

Prepare a clean repository whose `HEAD` has exactly one tag. The manifest version must equal the tag with one optional leading `v`: versions `1.4.0` and tags `1.4.0` or `v1.4.0` agree; `vv1.4.0` does not.

```shell
acr publish [PATH]
```

`PATH` defaults to `.`. The command runs these stages in order:

1. Load the manifest and enumerate every declared package file.
2. Require a clean worktree and exactly one tag at `HEAD`.
3. Match the tag to the manifest version.
4. Read package bytes and modes from the tagged Git tree and build the release assets.
5. Extract the archive and run apply plus idempotent check realization for Claude Code, Codex, and Cursor in fresh projects.
6. Refuse an existing visible release, a missing remote tag, or a remote tag at another commit.
7. Create a draft, upload and re-download every asset, verify its SHA-256 digest, revalidate the remote tag, and publish the draft.

Use `--dry-run` on a tagged commit to rehearse stages 1–6 before uploading. It requires the same clean worktree, version-matching tag at `HEAD`, and pushed remote tag as a real publication, but performs no GitHub writes. An untagged pull-request head is not publishable and fails before archive construction:

```shell
acr publish path/to/package --dry-run --json
```

Authentication follows normal ACR GitHub credential discovery: `GH_TOKEN`, then `GITHUB_TOKEN`, `gh auth token`, and the Git HTTPS credential helper. Tokens are never included in release assets or diagnostics.

## Deterministic archive

The archive is `<repository>-<version>.tar.gz`. It contains the lexicographically sorted files from `manifest.PackageFiles` beneath one root directory. ACR writes PAX tar headers and best-compression gzip framing with these normalized fields:

| Field | Value |
| --- | --- |
| Entry type | Regular file only |
| Path separator | `/` |
| Modification time | Unix epoch |
| Access/change time | Unset |
| User/group IDs | `0` |
| User/group names | Empty |
| File mode | `0755` for Git mode `100755`; otherwise `0644` |

The Git blob is authoritative. Working-tree line-ending filters, local modes, timestamps, owners, entry order, and umask cannot enter the archive.

The metadata `contentHash` is distinct from asset checksums. It is the consumer lock digest over sorted paths, normalized modes, sizes, and individual file digests. `checksums.txt` contains plain SHA-256 digests of the archive and metadata assets in `sha256sum` format.

## Release assets

Every published release contains exactly three assets:

| Asset | Purpose |
| --- | --- |
| `<repository>-<version>.tar.gz` | Reproducible package archive for auditing |
| `acr-package.json` | Schema-versioned package, commit, file, archive, and adapter evidence |
| `checksums.txt` | SHA-256 digests for the archive and metadata |

The metadata contract is [`schemas/acr-package.schema.json`](../schemas/acr-package.schema.json). A release-based install checks supported metadata against the commit and content hash computed from the source tree. Metadata is additive: an older release without it remains installable, and a commit SHA install does not consult releases.

## Immutable failure handling

A non-draft release owns its tag permanently. `acr publish` returns `release_already_exists` and uploads nothing when the version already exists. A create race reported by GitHub receives the same refusal.

Uploads remain in a draft until every remote asset matches its local SHA-256 digest. Consumers do not see drafts. A retry may delete a same-tag draft only when its asset names are unique members of the three-asset ACR set and its downloaded `acr-package.json` bytes match the package prepared for the current tagged tree. An empty draft, a missing or mismatched ownership marker, or any other asset name returns `foreign_draft_release` and requires manual inspection.

`tag_not_pushed` means the local version tag is absent on GitHub. `tag_commit_mismatch` means the remote tag points at a commit other than local `HEAD`. Neither condition creates a release.

## Reusable GitHub Actions workflow

A package repository can call the workflow at an immutable commit SHA:

```yaml
name: Publish
on:
  push:
    tags: ["v*"]
permissions:
  contents: write
jobs:
  publish:
    uses: jbaruch/agentic-context-registry/.github/workflows/publish-package.yml@<commit-sha>
```

The workflow accepts `path` (default `.`), `dry-run` (default `false`), and `acr-version` (default `v0.1.0`). The `dry-run` input rehearses a tag-triggered publication without uploading assets; it does not make pull-request or other untagged refs publishable. The workflow rejects non-tag refs, checks out full tag history, installs that immutable ACR version, verifies `acr version`, and runs `acr publish`. It declares only `contents: write` and passes the caller's `GITHUB_TOKEN` through `GH_TOKEN`; it has no secrets input.
