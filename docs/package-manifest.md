# Package Manifest v1

Every ACR package has an `agent-plugin.yaml` file at its root. The manifest describes logical artifacts without naming Claude Code, Codex, Cursor, or any native destination path. Adapters map this model into their own layouts.

The machine-readable schema is [`schemas/agent-plugin.schema.json`](../schemas/agent-plugin.schema.json). A [minimal package](../examples/minimal/agent-plugin.yaml) and a [complete package](../examples/complete/agent-plugin.yaml) are checked in as executable examples.

## Identity and Versioning

`schemaVersion` is the integer version of this contract. Version 1 consumers reject every other value and every unknown field.

`name` is the stable lowercase `owner/package` identity. `version` is the package's semantic version. Publishing must verify that `version` matches the Git tag; the publishing workflow is outside this contract.

`source.repository` is the canonical `https://github.com/owner/package` URL and must match `name`. Resolved releases and commit hashes belong in the consumer lockfile rather than the authored package manifest.

## Artifact Model

Artifact IDs use lowercase kebab case and are unique across the whole package. IDs remain stable when an adapter changes a destination filename.

| Class | Source shape | Additional metadata |
| --- | --- | --- |
| `rules` | One regular file | Required activation policy |
| `skills` | Directory containing `SKILL.md` | The complete directory is one artifact |
| `scripts` | One regular file | None |
| `hooks` | One regular file | Canonical event and optional arguments |

Paths use normalized, package-relative POSIX syntax. Absolute paths, parent traversal, backslashes, symbolic links, missing paths, and special files are invalid. A skill path names a directory rather than only its `SKILL.md`; sibling scripts, references, and assets remain part of that skill.

Rule activation has two modes:

- `always` loads the rule without path filtering and has no `paths` entries.
- `paths` requires one or more unique, package-relative glob patterns.

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

Undeclared files outside skill directories are not published. Duplicate file references are included once. Archive order is the lexicographic order returned by `manifest.PackageFiles`; archive timestamps and modes are defined by the publishing implementation.

## Schema Evolution

Schema versions fail closed. A consumer does not guess how to interpret a newer version or ignore fields it does not understand. Any contract change that adds, removes, or changes a field increments `schemaVersion`; older schemas remain available so consumers can report an actionable upgrade path.

Package semantic versions and schema versions are independent. A package can publish many semantic versions using schema version 1.
