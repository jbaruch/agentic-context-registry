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

## Hook Contract

Package authors target [Claude Code’s command-hook contract](https://code.claude.com/docs/en/hooks), restricted to the six neutral events above. Adapters translate it in realized output; package hooks do not detect the agent to select a response shape.

Stdin is one JSON object. Common fields are `hook_event_name` (the Claude event spelling), `session_id`, `transcript_path` (possibly null), and `cwd` (the session directory). Agents may add fields. Event fields are:

| Neutral event | `hook_event_name` | Event fields |
| --- | --- | --- |
| `session-start` | `SessionStart` | `source` |
| `session-end` | `SessionEnd` | `reason` |
| `user-prompt-submit` | `UserPromptSubmit` | `prompt` |
| `pre-tool-use` | `PreToolUse` | `tool_name`, `tool_input`, `tool_use_id` |
| `post-tool-use` | `PostToolUse` | Tool fields plus `tool_response` where supplied |
| `stop` | `Stop` | `stop_hook_active`, plus `last_assistant_message` where supplied |

`CLAUDE_PROJECT_DIR` is the absolute ACR project root containing `agents.yaml`. Process cwd is unspecified; use the environment variable for project paths. The environment is inherited, with that variable set by the translator for Codex and Cursor.

Tool names and the shapes of `tool_input` and `tool_response` are **not normalized**. A guard must handle the native tools it intends to govern; an edit is not necessarily an `Edit` call or a `file_path` argument.

| Agent | Native tool vocabulary examples |
| --- | --- |
| Claude Code | `Bash`, `Edit`, `Write` |
| Codex | `Bash`, `apply_patch` (patch in `tool_input.command`) |
| Cursor | `Shell`, `Read`, `Write` |

Exit `0` accepts empty stdout, plain text, or one JSON object. Plain text supplies context only for `session-start` and `user-prompt-submit`. Exit `2` requests block/deny with stderr as the reason: before-tool denies the tool, prompt submission blocks the prompt, stop continues the response, and after-tool adds feedback; session-start/end are advisory. Any other exit is a hook failure. Codex and Cursor translators deny gating events (`pre-tool-use`, `user-prompt-submit`) on every translation or runtime failure, including missing Python, unrunnable hooks and invalid object JSON. Other events report a non-blocking diagnostic; a failed Stop translator allows the stop without requesting continuation. Failed-hook stdout is discarded. Claude Code retains its native failure semantics.

Codex and Cursor realized definitions include an explicit native timeout. The Python translator keeps the package hook in the native command’s process group. On watchdog expiry or catchable cancellation it sends group SIGTERM only when the translator itself or the realized outer wrapper leads that group, protecting itself and its buffering shells with caught handlers without package hooks inheriting ignored SIGTERM. Otherwise SIGTERM targets only the direct hook; grandchildren receive no signal and may survive even with default SIGTERM handling. After a 0.2-second grace it SIGKILLs and reaps only the direct hook, closes capture pipes and discards partial output; it never SIGKILLs the shared group. Codex’s native cancellation reaches descendants remaining in the command group while the invocation is outstanding. SIGTERM-ignoring grandchildren may survive translator-originated cleanup indefinitely and perform side effects after its reply, with no guaranteed later host cleanup. This differs from descendants deliberately leaving the group, which also escape group cancellation. Codex’s native group SIGKILL and completed-command cleanup boundary are cited in [adapter behavior](adapters.md#hook-delivery); Cursor’s native cancellation implementation remains unverified, and synthetic tests provide no vendor guarantee. Interpreter startup hangs are outside the watchdog; shell guards still handle missing or failing `python3`. Supervision lasts until both output streams reach EOF; timeouts discard partial output. Gates deny on timeout and Stop allows termination. Each reply is checked against the native event contract before emission, including Codex's numeric and nesting limits described in [adapter behavior](adapters.md#hook-delivery). An unrepresentable allow with `updatedInput` denies the call with an ACR message naming the hook, event, runtime failure and recovery. Codex deny/block replies and exit-2 stderr always carry a non-empty trimmed reason, with an ACR message naming the hook, event, intentional outcome and policy review when the package omits one. Orphan reasons are removed on every Codex event, preserving supported additional context. A Codex PostToolUse reply with `continue:false` retains its reason. `continue:false` on Stop takes precedence over a block or additional context on both agents; Codex retains `stopReason` as prepared termination feedback and Cursor emits no followup. A Codex allow without `updatedInput` becomes an accepted no-op.

For intentional outcomes, Codex and Cursor prepare all accompanying feedback, including stderr: unsafe control characters are escaped and rendered detail is bounded to 16 KiB before adding hook identity, outcome and recovery. Successful context and ordinary successful stderr remain unchanged. Filtering diagnostics do not echo unsupported field names.

The portable output fields are:

- `hookSpecificOutput.additionalContext` on every event except `session-end`, subject to the native delivery limits below.
- `hookSpecificOutput.permissionDecision`: `allow`, `deny`, or `ask`, with `permissionDecisionReason` and optional `updatedInput`, on `pre-tool-use`.
- Top-level `decision: "block"` plus `reason` on `user-prompt-submit`, `post-tool-use`, and `stop`.

`hookSpecificOutput.hookEventName` names the canonical event. Other Claude fields reach only hosts that consume them natively; translation drops unsupported fields with stderr diagnostics. Codex and Cursor cannot enforce `ask`, so ACR denies the call and directs the agent to ask in the conversation before retrying. A post-tool block supplies feedback; it cannot undo the tool call.

| Delivery | [Claude Code](https://code.claude.com/docs/en/hooks) | [Codex](https://developers.openai.com/codex/hooks) | [Cursor](https://cursor.com/docs/hooks) |
| --- | --- | --- | --- |
| Canonical input | Native | Bytes unchanged; project env supplied | Adds canonical event/session/cwd/source, tool response, stop guard; retains native fields |
| Context | Native fields or permitted text | Native context; after-tool block becomes appended context | `additional_context` at session start/after tool; `followup_message` at stop; prompt and before-tool context dropped with diagnostic |
| Tool permission | Native | `ask` → deny; `defer` removed; unsupported stop/suppression fields removed | Native `permission` and messages, `updated_input`; empty decision → allow; `ask` → deny |
| Blocking | Native decisions and exit 2 | Native decisions and exit 2; after-tool feedback preserves original tool result | Native prompt `continue:false`, tool deny, stop followup, after-tool context |
| Runtime | Package executable | `python3` ≥ 3.9 on PATH | `python3` ≥ 3.9 on PATH |

See [adapter behavior](adapters.md#hook-delivery) for watchdog budgets and vendor citations, dropped fields, runtime requirements, trust and co-selection caveats. Permission UI and tool vocabulary remain native-agent behavior.

## Validation

JSON Schema validation covers the manifest shape, required fields, formats, and enumerated values. `internal/manifest` adds filesystem and cross-artifact checks that JSON Schema cannot express.

Validation failures carry stable codes:

| Code | Meaning |
| --- | --- |
| `unsupported_schema_version` | `schemaVersion` is not supported |
| `no_artifacts` | The manifest declares no rule, skill, script, or hook |
| `duplicate_artifact_id` | An ID appears more than once across artifact classes |
| `path_not_found` | A declared file, directory, skill `SKILL.md`, or provider-selected implicit root notice is missing |
| `invalid_path` | A selected path violates the archive lexical contract, or file or directory paths collide under canonical normalization and Unicode full case folding |
| `invalid_artifact_type` | A declared path has the wrong kind or crosses a symbolic link; an implicit root license has a nonregular type or cannot be inspected |
| `invalid_skill_tree` | A skill contains a symbolic link or special file |
| `unsupported_hook_event` | A hook event is outside the v1 vocabulary |

Declaration validation reports semantic and filesystem failures in manifest order. ACR checks authored paths for aliases even when one spelling cannot be opened locally. Alias checks sort and exact-deduplicate leaves before checking each leaf and its ancestors, so reordered inventories produce the same diagnostics.

## Published Files

A package archive contains only:

1. `agent-plugin.yaml`.
2. Every declared rule, script, and hook file.
3. Every regular file recursively contained by a declared skill directory.
4. Every regular file directly in the package root with one of these exact, case-sensitive names: `LICENSE`, `LICENSE.md`, `LICENSE.txt`, `LICENCE`, `LICENCE.md`, `LICENCE.txt`, `COPYING`, `COPYING.md`, `COPYING.txt`, `UNLICENSE`, `UNLICENSE.md`, `UNLICENSE.txt`. Original names and bytes are preserved. Matching directories are skipped without traversal; selected symbolic links, gitlinks, and special files are rejected. Other spellings and suffixes are not implicitly included; rename them to a supported literal or carry them through declared content such as a skill support file.

Inside a Git work tree, `acr validate` selects each declaration directly with `git --no-replace-objects --literal-pathspecs ls-files -z --cached --others --exclude-standard`. Skill descendants come from Git's index plus untracked files that no ignore rule excludes. Ignored descendants, such as `__pycache__/*.pyc`, never reach local file inspection. Git supplies the exact path spelling; ACR does not replace Git names with filesystem spellings or rewrite Unicode or case. Every selected local file and its ancestors must be regular files or directories, with no symbolic links. A tracked file deleted locally is still selected and reported as `path_not_found`.

Untracked files that are not ignored remain visible to validation; publication refuses the dirty worktree. The manifest and declared rule, script, hook, and `SKILL.md` files remain required even when absent from Git's inventory. A declared ignored file can pass local validation when present on disk, but publication refuses it if absent from the committed tree. Check the tagged tree for the exact committed spelling first, even when the declaration matches an ignore rule. If the spelling is intentional, either remove the declaration or change the ignore rule and track the file. Commit the change and retag. Force-tracked ignored files remain publishable.

Publication first performs the same local preflight, then selects the distribution from the resolved commit bound to the version tag. It reads each selected regular blob by its object ID, preserving committed bytes, modes and exact Git names even if a tag moves during preparation. Missing declared entries and unsupported declared tree modes produce `unpublishable_path`. Implicit root-license type and metadata failures use the same manifest validation diagnostics as local preflight. Git command failures, malformed output, and unavailable or corrupt blobs produce `git_access_failed` with repository access/integrity advice.

Every declaration must match the selected Git path byte for byte, including skill roots and required `SKILL.md` files. If Git returns a differently spelled entry, the `unpublishable_path` diagnostic shows both spellings escaped, such as declared `"cafe\u0301.md"` versus Git's `"caf\u00e9.md"`. If the literal lookup finds no entry, check the tagged tree for the exact committed spelling. If the file is already committed under a different spelling, correct the declaration in `agent-plugin.yaml`, commit and retag before publication. Exact NFD names are valid when Git stores those bytes. Filesystem aliases can let local validation succeed despite a mismatched declaration. Publication preserves the committed manifest bytes so both release archives and source installs use the same declarations. The shared Git runner disables replacement objects for validation and publication, including identity, tree selection and blob reads.

Outside a Git work tree (no repository around the directory, a bare repository, or no `git` executable), validation retains the full-directory filesystem walk. Other Git failures, including configuration, permission and `safe.directory` failures, stop the operation instead of enabling that fallback. Publishing still requires Git and a clean tagged checkout.

Root licenses follow the same Git selection as artifacts: ignored untracked notices are excluded, force-tracked notices are included, and tracked deletions are errors. Publication discovers notices with a nonrecursive listing of the tagged commit's root. Outside Git, the filesystem root is listed without traversing matching directories. Every selected distribution leaf must satisfy the archive's normalized relative POSIX path contract, including its backslash and NUL exclusions.

ACR compares each file and directory ancestor by canonically decomposing its name, applying Unicode full case folding, and recomposing to NFC. The comparison also retains simple case pairs from Go's newer Unicode tables. Distinct spellings sharing a key produce `invalid_path`, as does a file and directory occupying the same key. For example, `ss` and `ß`, composed `café` and decomposed `cafe\u0301`, and `A/x` and `a/y` conflict. Either Unicode spelling is valid on its own, and files can share a directory with the same exact spelling. Archives preserve original paths and bytes. Rename conflicting files or directories and update the declarations, then commit and retag before publishing. Publication also creates temporary files for hashing exclusively, so any collision reported by the local filesystem aborts publication before it can return metadata. See [issue #221](https://github.com/jbaruch/agentic-context-registry/issues/221).

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
