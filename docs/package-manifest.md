# Package Manifest v1

Every ACR package has an `agent-plugin.yaml` file at its root. The manifest describes logical artifacts without naming Claude Code, Codex, Cursor, or any native destination path. Adapters map this model into their own layouts.

The machine-readable schema is [`schemas/agent-plugin.schema.json`](../schemas/agent-plugin.schema.json). A [minimal package](../examples/minimal/agent-plugin.yaml) and a [complete package](../examples/complete/agent-plugin.yaml) are checked in as executable examples.

## Identity and Versioning

`schemaVersion` is the integer version of this contract. Version 1 consumers reject every other value and every unknown field.

`name` is the stable lowercase `owner/package` identity. `version` is the package's semantic version. Publishing must verify that `version` matches the Git tag; the publishing workflow is outside this contract.

`source.repository` is the canonical `https://github.com/owner/package` URL and must match `name`. `source.tesslIdentity` is optional and records the `<workspace>/<package>` a Tessl consumer installed the package under, so its own `.tessl/plugins/<identity>/...` references resolve after migration; only migration writes it, and only a reference naming a recorded or matching identity is ever rebased (see [`docs/adapters.md`](adapters.md)). Written out, it must be a package identity: `tesslIdentity: ""`, `tesslIdentity: null` and an empty value are all refused, and omitting the key entirely is how a package declares that it has none. Resolved releases and commit hashes belong in the consumer lockfile rather than the authored package manifest.

## Artifact Model

Artifact IDs use lowercase kebab case and are unique across the whole package. IDs remain stable when an adapter changes a destination filename.

| Class | Source shape | Additional metadata |
| --- | --- | --- |
| `rules` | One regular file | Required activation policy |
| `skills` | Directory containing `SKILL.md` | The complete directory is one artifact |
| `scripts` | One regular file | None |
| `hooks` | One regular file | Canonical event and optional arguments |

Paths use normalized, package-relative POSIX syntax. Absolute paths, Windows drive prefixes, parent traversal, backslashes, symbolic links, missing paths, and special files are invalid. A skill path names a directory rather than only its `SKILL.md`; sibling scripts, references, and assets remain part of that skill.

Rule activation has two modes:

- `always` loads the rule without path filtering and has no `paths` entries.
- `paths` requires one or more unique glob patterns relative to the consuming project. Artifact storage paths do not prefix these activation patterns.

## Hook Vocabulary

Hook event names are lowercase and agent-neutral. An adapter reports an unsupported event before realization and owns the exact native spelling or configuration structure.

| Event | Meaning |
| --- | --- |
| `session-start` | A new agent session starts |
| `session-end` | An agent session ends |
| `user-prompt-submit` | The user submits a prompt |
| `pre-tool-use` | Immediately before a tool invocation |
| `post-tool-use` | Immediately after a tool invocation |
| `stop` | The agent is about to stop its current response |

## Validation

JSON Schema validation covers the manifest shape, required fields, formats, and enumerated values. `internal/manifest` adds filesystem and cross-artifact checks that JSON Schema cannot express.

Validation failures carry stable codes:

| Code | Meaning |
| --- | --- |
| `unsupported_schema_version` | `schemaVersion` is not supported |
| `no_artifacts` | The manifest declares no rule, skill, script, or hook |
| `duplicate_artifact_id` | An ID appears more than once across artifact classes |
| `path_not_found` | A declared file, directory, or skill `SKILL.md` is missing |
| `invalid_path` | A path is absolute, non-normalized, or traverses a parent |
| `invalid_artifact_type` | A declared path has the wrong kind or crosses a symbolic link |
| `invalid_skill_tree` | A skill contains a symbolic link or special file |
| `unsupported_hook_event` | A hook event is outside the v1 vocabulary |

The validator returns failures in manifest order so repeated checks produce the same diagnostics.

## Published Files

A package archive contains only:

1. `agent-plugin.yaml`.
2. Every declared rule, script, and hook file.
3. Every regular file recursively contained by a declared skill directory.
4. Every regular file directly in the package root whose basename starts with `LICENSE`, `LICENCE`, `COPYING`, or `UNLICENSE`, matching ASCII case insensitively (for example, `License-MIT.txt`). Original names and bytes are preserved. Matching directories are skipped without traversal; selected symbolic links, gitlinks, and special files are rejected.

Inside a Git work tree, `acr validate` selects each declaration directly with `git --no-replace-objects --literal-pathspecs ls-files -z --cached --others --exclude-standard`. Skill descendants come from Git's index plus untracked files that no ignore rule excludes. Ignored descendants, such as `__pycache__/*.pyc`, never reach local file inspection. Git supplies the exact path spelling; ACR does not reconcile a directory walk or normalize Unicode or case. Every selected local file and its ancestors must be regular files or directories, with no symbolic links. A tracked file deleted locally is still selected and reported as `path_not_found`.

Untracked files that are not ignored remain visible to validation; publication refuses the dirty worktree. The manifest and declared rule, script, hook, and `SKILL.md` files remain required even when absent from Git's inventory. A declared ignored file can pass local validation when present on disk, but publication refuses it if absent from the committed tree. The diagnostic offers removing the declaration or intentionally changing the ignore rule. Force-tracked ignored files remain publishable.

Publication first performs the same local preflight, then selects the distribution from the resolved commit bound to the version tag. It reads each selected regular blob by its object ID, preserving committed bytes, modes and exact Git names even if a tag moves during preparation. Missing required entries and unsupported tree modes produce `unpublishable_path`. Git command failures, malformed output, and unavailable or corrupt blobs produce `git_access_failed` with repository access/integrity advice.

Every declaration must match the selected Git path byte for byte, including skill roots and required `SKILL.md` files. A mismatch returns `unpublishable_path` with both spellings escaped: for example, declared `"cafe\u0301.md"` versus Git's `"caf\u00e9.md"`. Correct `agent-plugin.yaml` to the committed Git spelling, commit and retag before publication. Exact NFD names are valid when Git stores those bytes. Filesystem aliases can let local validation succeed despite a mismatched declaration; that success does not establish publication readiness. Publication preserves the committed manifest bytes so both release archives and source installs use the same declarations. The shared Git runner disables replacement objects for validation and publication, including identity, tree selection and blob reads.

Outside a Git work tree (no repository around the directory, a bare repository, or no `git` executable), validation retains the full-directory filesystem walk. Other Git failures, including configuration, permission and `safe.directory` failures, stop the operation instead of enabling that fallback. Publishing still requires Git and a clean tagged checkout.

Root licenses follow the same Git selection as artifacts: ignored untracked notices are excluded, force-tracked notices are included, and tracked deletions are errors. Publication discovers notices with a nonrecursive listing of the tagged commit's root. Outside Git, the filesystem root is listed without traversing matching directories.

These distribution checks apply to validation and publication. Local dependency snapshots use only the manifest and declared-artifact content closure; they do not enumerate, open or reject implicit root notices, including symlinks and special files. Explicitly declared licenses retain ordinary artifact validation.

Other undeclared files outside skill directories are not published. Exact duplicate references, including overlapping skills or a root license also declared as an artifact, are included once. Distribution paths are sorted lexicographically.

The distribution inventory is larger than the v1 `contentHash` closure. That digest continues to cover only `agent-plugin.yaml` and declared artifacts, so existing locks remain valid even when their source tree already contains an undeclared root license. An explicitly declared license remains hashed. Implicit notices appear in archive checksums and the metadata's full file inventory, but consumers verify commit and `contentHash`, not supplementary files against that inventory. Root notices receive no artifact identity, activation policy, native output, or realization-ledger entry. This archive behavior does not install notices into native agent layouts; licenses within declared skills continue to render as support files.

The publishing archive normalizes host-dependent metadata:

| Property | Published value |
| --- | --- |
| Root | `<repository>-<version>/` |
| Entries | Regular files only; no directory or link entries |
| Paths | Package-relative POSIX paths |
| Timestamps | Unix epoch modification time; no access or change time |
| Owners | Numeric user and group `0`; empty owner names |
| Modes | `0755` for Git tree mode `100755`; `0644` for `100644` |

Content and modes come from the tagged Git tree rather than the working tree. Checkout line-ending conversion, local permission changes, file timestamps, ownership, entry order, and process umask do not change the archive bytes.

## Schema Evolution

Schema versions fail closed. A consumer does not guess how to interpret a newer version or ignore fields it does not understand. Any contract change that adds, removes, or changes a field increments `schemaVersion`; older schemas remain available so consumers can report an actionable upgrade path.

Package semantic versions and schema versions are independent. A package can publish many semantic versions using schema version 1.
