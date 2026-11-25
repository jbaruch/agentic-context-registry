# Publishing packages

ACR publishes immutable package evidence as GitHub Release assets. The tagged Git source tree remains the installable package: `acr install github:owner/repository[@TAG|@SHA]` downloads the repository archive at the resolved commit and does not require a hosted registry or discovery service.

## Publish a package

Prepare a clean repository whose `HEAD` has exactly one tag. The manifest version must equal the tag with one optional leading `v`: versions `1.4.0` and tags `1.4.0` or `v1.4.0` agree; `vv1.4.0` does not.

```console
$ acr publish --dry-run
# fixture: publisher
# exit: 0
Release v1.0.0 is publishable with 3 assets; rerun without --dry-run to upload it.
```

`PATH` defaults to `.` in the selected `--project`, or the current directory when no project is supplied. Relative package paths resolve under that project; relative project paths resolve from the current directory. The selected project must exist and permit directory read and search access; after that check, an absolute package path selects the package independently of the project. This selection applies to both dry-run and publication. The command runs these stages in order:

1. Load the manifest and select local package files directly from Git’s index plus nonignored untracked files using literal pathspecs. Files under a skill directory that Git ignores, such as `__pycache__/*.pyc`, are not enumerated, so stray bytecode from local test runs cannot block a publish; see [Published files](package-manifest.md#published-files).
2. Require a clean worktree and exactly one tag at `HEAD`.
3. Match the tag to the manifest version.
4. Select package entries from the resolved tagged commit, require declarations to match Git's exact path bytes, read regular blob bytes by object ID, preserve committed modes and manifest bytes, and build the release assets.
5. Extract the archive and run apply plus idempotent check realization for Claude Code, Codex, and Cursor in fresh projects.
6. Refuse an existing visible release, a missing remote tag, or a remote tag at another commit.
7. Create a draft, upload and re-download every asset, verify its SHA-256 digest, revalidate the remote tag, and publish the draft.

Use `--dry-run` on a tagged commit to rehearse stages 1–6 before uploading. It requires the same clean worktree, version-matching tag at `HEAD`, and pushed remote tag as a real publication, but performs no GitHub writes. An untagged pull-request head is not publishable and fails before archive construction.

Use `acr publish path/to/package --dry-run --json` when automation needs the same plan as structured output.

Authentication follows normal ACR GitHub credential discovery: `GH_TOKEN`, then `GITHUB_TOKEN`, `gh auth token`, and the Git HTTPS credential helper. Tokens are never included in release assets or diagnostics.

## Dual-publishing

During a registry transition, keep `agent-plugin.yaml` beside `.tessl-plugin/plugin.json` (and `tile.json` when the package already uses it). Artifact source files stay shared; each manifest describes the same version for its own consumer. `acr publish` includes files reachable from `agent-plugin.yaml` plus matching root licenses, so Tessl manifests remain in the tagged repository but do not enter the ACR package archive.

Use one version and one Git tag for both distribution paths. Convert and review `agent-plugin.yaml` first with the [producer migration command](migration-producer.md), commit both manifests and their shared sources, run each ecosystem's validation, create the single tag, publish Tessl's artifact, and then run `acr publish` for the immutable ACR release. This is the #11 producer-conversion step followed by the #9 publication step; consumers begin the [migration guide](migration-guide.md) only after that release is visible.

If the two manifests cannot truthfully describe one version, do not reuse the tag. Correct the source or bump the version, regenerate `agent-plugin.yaml`, and publish a new immutable tag. Retiring the Tessl manifest is a later producer release decision, independent of consumer finalization.

## Deterministic archive

The archive is `<repository>-<version>.tar.gz`. It contains the lexicographically sorted distribution beneath one root directory: the manifest, every declared rule, script and hook, regular descendants of declared skills, and regular root files named exactly `LICENSE`, `LICENSE.md`, `LICENSE.txt`, `LICENCE`, `LICENCE.md`, `LICENCE.txt`, `COPYING`, `COPYING.md`, `COPYING.txt`, `UNLICENSE`, `UNLICENSE.md`, or `UNLICENSE.txt` (case sensitive). `manifest.SelectListed` selects declarations directly from the resolved commit's tree using literal pathspecs and discovers root notices through a nonrecursive root listing. Matching directories are skipped, unsupported file modes are rejected, and exact duplicate paths appear once. Git selection preserves original names without a filesystem walk. ACR returns `invalid_path` if any selected file violates the archive path syntax or if files or directory ancestors collide under [canonical normalization and Unicode full case folding](package-manifest.md#published-files). Rename conflicting source paths and update declarations before committing and retagging. Validation never rewrites Git names. Blob reads use the selected object IDs, so a moving local tag cannot change the content being read. On a publishable clean tagged checkout the archive entries equal the validation inventory, with Git's exact spelling. Unrelated repository files are excluded. ACR writes PAX tar headers and best-compression gzip framing with these normalized fields:

| Field | Value |
| --- | --- |
| Entry type | Regular file only |
| Path separator | `/` |
| Modification time | Unix epoch |
| Access/change time | Unset |
| User/group IDs | `0` |
| User/group names | Empty |
| File mode | `0755` for Git mode `100755`; otherwise `0644` |

The Git blob is authoritative. Working-tree line-ending filters, local modes, timestamps, owners, entry order, and umask cannot enter the archive. All validation and publication Git commands use `git --no-replace-objects`, so replace refs cannot substitute the identity, selected tree or blob bytes.

Declarations, including skill roots and required `SKILL.md`, must match Git's selected names byte for byte. Support descendants retain Git's spelling even when the local disk uses aliases. Local validation can succeed through a filesystem alias while publication returns `unpublishable_path`: for example, declared `"cafe\u0301.md"` versus Git's `"caf\u00e9.md"`. Correct the committed declaration in `agent-plugin.yaml`, commit and retag. ACR preserves the manifest bytes and Git archive names; it does not require NFC when Git itself stores NFD names.

The metadata `contentHash` is distinct from asset checksums. Its v1 closure remains the manifest and declared artifacts, over sorted paths, normalized modes, sizes, and individual file digests. Implicit root notices are excluded from that digest; a notice explicitly declared as an artifact remains included. Metadata `files` inventories the full distribution, including notices, with mode, size and SHA-256. Changing only an implicit notice changes archive and metadata asset checksums without changing `contentHash`. `checksums.txt` contains plain SHA-256 digests of the archive and metadata assets in `sha256sum` format.

## Release assets

Every published release contains exactly three assets:

| Asset | Purpose |
| --- | --- |
| `<repository>-<version>.tar.gz` | Reproducible package archive for auditing |
| `acr-package.json` | Schema-versioned package, commit, file, archive, and adapter evidence |
| `checksums.txt` | SHA-256 digests for the archive and metadata |

The metadata contract is [`schemas/acr-package.schema.json`](../schemas/acr-package.schema.json). A release-based install checks supported metadata against the commit and content hash computed from the source tree; it does not verify supplementary notice bytes against metadata `files`. Metadata is additive: an older release without it remains installable, and a commit SHA install does not consult releases. Root notices are distribution content, not realized artifacts: they add no native output or ledger entry. Notices inside declared skills retain their existing support-file behavior.

## Immutable failure handling

A non-draft release owns its tag permanently. `acr publish` returns `release_already_exists` and uploads nothing when the version already exists. A create race reported by GitHub receives the same refusal.

Existing locks and historical archives remain valid, including archives that omitted an undeclared notice present in their source tree. Recomputed distribution inventories gain that notice without changing the v1 content hash. Do not republish those releases: publish a new package version to ship a license-bearing archive. Neither the manifest schema nor metadata version changes for this inclusion rule.

Existing local locks also remain valid: local snapshots contain only the manifest and declared artifacts, excluding implicit root notices regardless of type. An undeclared root notice that is a symlink or special file is neither enumerated nor opened during local materialization or realization. An explicitly declared license still follows ordinary artifact validation and contributes to the content hash.

Uploads remain in a draft until every remote asset matches its local SHA-256 digest. Consumers do not see drafts. A retry may delete a same-tag draft only when its asset names are unique members of the three-asset ACR set and its downloaded `acr-package.json` bytes match the package prepared for the current tagged tree. An empty draft, a missing or mismatched ownership marker, or any other asset name returns `foreign_draft_release` and requires manual inspection.

For `acr publish`, `tag_not_pushed` means the local version tag is absent on GitHub, and `tag_commit_mismatch` means the remote tag points at a commit other than local `HEAD`. Neither condition creates a release.

## CLI release notes

The release operator creates an annotated `vMAJOR.MINOR.PATCH` tag on the merged release commit. Put the release notes after a blank line below the annotation subject. The second `-m` starts a new paragraph.

```sh
git tag -a v0.7.0 -m 'acr v0.7.0' -m 'Record the changes users need to know.'
git show --no-patch v0.7.0
```

The CLI release guard reads the notes from the fetched tag and returns `release_notes_missing` for a lightweight tag or an empty annotation body. The tag itself records the notes, so release PRs need no `CHANGELOG.md` edit. Earlier tags retain the release workflow from their tagged source.

The CLI guard returns `tag_commit_mismatch` if the tag is missing from the local checkout, Git cannot read it, or Git returns unexpected annotation data. An annotated tag with release notes also gets this error if it resolves to a non-commit object or to a commit that differs from the workflow commit.

The guard compares the fetched annotation object's SHA with the pushed tag ref. The publisher checks the same approved SHA against its checkout and the pushed ref before creating a draft, then rechecks the pushed ref before making the draft visible. Replacing the annotation object returns `tag_commit_mismatch` even when the tag still points to the same commit.

Set `tag` to the failed run's `GITHUB_REF_NAME` and inspect it in the tagged checkout.

```sh
tag=v0.7.0
git --no-replace-objects tag --list --format='%(objectname) %(*objecttype) %(*objectname)' -- "$tag"
git ls-remote --refs origin "refs/tags/$tag"
```

The first command should show the annotation object's SHA, `commit`, and the full `GITHUB_SHA` from that run. The remote ref should show the same annotation object's SHA. If Git reports an error, check that the checkout can read the tag and its objects. If the tag is missing, fetch it with `git fetch origin tag "$tag"` and rerun the check. If the local copy may be stale, fetch the tag in a fresh checkout and compare again.

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
