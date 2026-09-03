# Troubleshooting

Use the structured `error.code` or notice `code` as the lookup key. Message text may add paths, fields, and recovery evidence; this page does not duplicate those runtime strings. Exit `1` is operational, `2` is usage, `3` is unapplied change, and `4` is a preservation or ownership refusal. Resolve an exit-4 cause before retrying; `--force` never grants ownership.

## Conflicts and ownership

| Symptom | Exit | Error code | Remedy | Owning page |
| --- | --- | --- | --- | --- |
| A shared JSON or TOML entry cannot be merged safely | 4 | `config_conflict` | Restore the last known managed entry, then run `acr check` | [Preservation](preservation.md#json-and-toml) |
| Two contributions claim one structural configuration identity | 4 | `duplicate_config_entry` | Rename one artifact or native key, then run `acr realize --dry-run` | [Adapter output kinds](adapters.md#output-kinds) |
| A vendored package differs from the explicit upstream mapping | 4 | `effective_mismatch` | Review both packages, then run `acr migrate tessl --map FROM=SOURCE` with equivalent content | [Vendored packages](migration.md#vendored-packages) |
| Finalization cannot prove every safety gate | 4 | `finalization_blocked` | Apply the error object's `remedy` when present, then run `acr migrate tessl --finalize --dry-run` | [Finalization](migration.md#finalization-and-recovery) |
| A finalization splice no longer matches its observed hash | 4 | `finalization_conflict` | Restore or reconcile the named file, then run `acr migrate tessl --finalize --dry-run` | [Finalization](migration.md#finalization-and-recovery) |
| A finalization transaction fails after its gates pass | 1 | `finalization_failed` | Run `acr migrate tessl --finalize` again to recover the journal | [Finalization](migration.md#finalization-and-recovery) |
| A managed Markdown marker is malformed, edited, duplicated, or unowned | 4 | `marker_conflict` | Restore the recorded marker span, then run `acr check` | [Markdown blocks](preservation.md#markdown-blocks) |
| Existing or ledger ownership cannot provide a non-vacuous proof | 4 | `ownership_conflict` | Preserve the unmanaged content in a regular file, then run `acr realize --dry-run` | [Ownership transitions](preservation.md#ownership-transitions-and-removal) |
| Realization detects any unsafe managed/unmanaged transition | 4 | `realization_conflict` | Resolve the reported target, then run `acr check` | [Safe removal](realization.md#safe-removal) |
| A plan targets an installed Tessl-owned path | 4 | `tessl_owned_target` | Change the package destination or complete `acr migrate tessl --finalize` first | [Tessl migration](migration.md#command) |
| A staged or existing vendor destination has different bytes | 4 | `vendor_collision` | Inspect the named tree, move conflicting bytes, then run `acr migrate tessl --vendor-unmapped` | [Vendored packages](migration.md#vendored-packages) |

## Missing source mappings and migration

| Symptom | Exit | Error code | Remedy | Owning page |
| --- | --- | --- | --- | --- |
| Both plain and `v`-prefixed tags match one Tessl version | 1 | `ambiguous_tessl_version` | Choose one explicitly with `acr migrate tessl --map FROM=SOURCE@TAG` | [Package mappings](migration.md#package-mappings) |
| CLI, file, or manifest mappings disagree | 1 | `mapping_conflict` | Remove the lower-priority conflict, then run `acr migrate tessl --dry-run` | [Package mappings](migration.md#package-mappings) |
| A mapping YAML file cannot be read or decoded | 1 | `mapping_file_invalid` | Fix the named file, then run `acr migrate tessl --mapping-file PATH --dry-run` | [Package mappings](migration.md#package-mappings) |
| Producer and consumer manifests cannot be converted through a common field value | 1 | `ambiguous_manifest` | Reconcile the two Tessl manifests, then run `acr migrate tessl-plugin --dry-run` | [Producer inputs](migration-producer.md#inputs-and-output) |
| Producer conversion would make a native hook fire on more agents | 1 | `agent_widening` | Move the hook to consensus configuration or run `acr migrate tessl-plugin --accept-agent-widening` | [Producer report](migration-producer.md#report-and-exit-codes) |
| Existing `agent-plugin.yaml` differs from the conversion result | 1 | `manifest_conflict` | Review and remove the old file, then run `acr migrate tessl-plugin` | [Producer idempotency](migration-producer.md#idempotency-and-dual-manifests) |
| A mapped repository has no ACR release (a release-resolution 404), or migration cannot complete another uncategorized operation | 1 | `migrate_failed` | For a release-resolution 404 on a public repository, complete [producer stage 0](migration-guide.md#stage-0-prepare-and-publish-the-producer) and run `acr publish`; for another cause, run `acr migrate tessl --dry-run --json`, inspect it, and retry | [Migration command](migration.md#command) |
| `tessl.json` is absent or not a regular file while Tessl state remains | 1 | `tessl_manifest_absent` | Restore `tessl.json`, then run `acr migrate tessl --dry-run` | [Migration command](migration.md#command) |
| A mapped release archive has no ACR manifest, unlike the no-release `migrate_failed` case | 1 | `source_not_a_package` | In the producer, run `acr migrate tessl-plugin`, publish it, then retry migration | [Producer conversion](migration-producer.md) |
| Neither accepted release tag matches the installed Tessl version | 1 | `tessl_version_unavailable` | Supply the intended tag with `acr migrate tessl --map FROM=SOURCE@TAG` | [Package mappings](migration.md#package-mappings) |
| Conversion finds a Tessl field with no safe v1 mapping | 1 | `unmapped_field` | Remove or migrate the named field, then run `acr migrate tessl-plugin --dry-run` | [Producer mapping](migration-producer.md#mapping) |
| Conversion finds an unknown Tessl manifest field | 1 | `unknown_field` | Remove or correct the field, then run `acr migrate tessl-plugin --dry-run` | [Producer mapping](migration-producer.md#mapping) |
| A Tessl package has no repository evidence | 1 | `unmapped_package` | Run `acr migrate tessl --map FROM=github:owner/repo` or `acr migrate tessl --vendor-unmapped` | [Package mappings](migration.md#package-mappings) |
| A vendor source tree escapes its package boundary or contains a link | 1 | `vendor_escape` | Replace unsafe entries with regular files, then run `acr migrate tessl --vendor-unmapped` | [Vendored packages](migration.md#vendored-packages) |
| Existing ACR state disagrees with the migration projection | 1 | `project_state_conflict` | Run `acr migrate tessl --dry-run --json`, then align or remove the disagreeing `agents.yaml` or `.agents/registry.lock` | [Migration guide](migration-guide.md) |

## Invalid includes and package content

A finding inside a selected instruction root's connected component aborts both `acr realize` and `acr check`. The same finding in an unrelated component stays a warning. The include error code appears in the diagnostic evidence while the CLI refusal is `realization_conflict` at exit `4`.

| Symptom | Exit | Error code | Remedy | Owning page |
| --- | --- | --- | --- | --- |
| Two direct or transitive imports reach the same file twice | 4 | `duplicate_include` | Remove the duplicate import, then run `acr check` | [Include graphs](preservation.md#include-graphs) |
| A selected import graph contains a cycle | 4 | `include_cycle` | Break the reported cycle, then run `acr check` | [Include graphs](preservation.md#include-graphs) |
| An import target is absolute, escaping, malformed, or otherwise invalid | 4 | `invalid_include` | Replace it with one normalized relative path, then run `acr check` | [Include graphs](preservation.md#include-graphs) |
| A selected import target does not exist | 4 | `unresolved_include` | Create the intended file or remove the import, then run `acr check` | [Include graphs](preservation.md#include-graphs) |
| Two activation globs in one rule are identical | 1 | `duplicate_activation_path` | Remove the duplicate glob, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| Artifact IDs repeat across one package | 1 | `duplicate_artifact_id` | Rename one ID, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| An artifact ID is not lowercase kebab case | 1 | `invalid_artifact_id` | Correct the ID, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| A declared artifact has the wrong filesystem type | 1 | `invalid_artifact_type` | Replace it with the required regular file or directory, then run `acr publish --dry-run` | [Validation](package-manifest.md#validation) |
| A package name is not a canonical lowercase owner/name | 1 | `invalid_package_name` | Correct `name`, then run `acr publish --dry-run` | [Identity](package-manifest.md#identity-and-versioning) |
| A package-relative path is unsafe or non-normalized | 1 | `invalid_path` | Correct the manifest path, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| A rule activation omits or contradicts its mode fields | 1 | `invalid_rule_activation` | Correct the activation object, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| A skill tree contains a link, special file, or missing `SKILL.md` | 1 | `invalid_skill_tree` | Replace unsafe entries with regular files, then run `acr publish --dry-run` | [Validation](package-manifest.md#validation) |
| `source.repository` is missing, malformed, or mismatched | 1 | `invalid_source` | Set the canonical URL, then run `acr publish --dry-run` | [Identity](package-manifest.md#identity-and-versioning) |
| `version` is not semantic version syntax | 1 | `invalid_version` | Correct the version, then run `acr publish --dry-run` | [Identity](package-manifest.md#identity-and-versioning) |
| A package declares no artifacts | 1 | `no_artifacts` | Declare at least one v1 artifact, then run `acr publish --dry-run` | [Artifact model](package-manifest.md#artifact-model) |
| A declared path does not exist | 1 | `path_not_found` | Add the file or correct the path, then run `acr publish --dry-run` | [Validation](package-manifest.md#validation) |
| A required manifest field is absent | 1 | `required` | Add the named field, then run `acr publish --dry-run` | [Validation](package-manifest.md#validation) |
| A hook event is outside the neutral v1 vocabulary | 1 | `unsupported_hook_event` | Choose a listed event, then run `acr publish --dry-run` | [Hook vocabulary](package-manifest.md#hook-vocabulary) |
| A state or manifest schema is newer or otherwise unsupported | 1 | `unsupported_schema_version` | Upgrade `acr`, then retry the original `acr` command | [Schema evolution](package-manifest.md#schema-evolution) |
| A published path contains excluded cache, VCS, or bytecode content | 1 | `unpublishable_content` | Remove the excluded content, then run `acr migrate tessl-plugin --dry-run` | [Producer report](migration-producer.md#report-and-exit-codes) |

## Adapter drift and realization

| Symptom | Exit | Error code | Remedy | Owning page |
| --- | --- | --- | --- | --- |
| The release archive fails adapter apply or idempotence verification | 1 | `adapter_realization_failed` | Run `acr realize --dry-run` in a fresh fixture and correct the package | [Publishing](publishing.md#publish-a-package) |
| One selected adapter does not support an artifact or event | 4 | `unsupported_adapter_capability` | Remove the unsupported selection or artifact, then run `acr realize --dry-run` | [Capability preflight](adapters.md#capability-preflight) |
| A generated hook or script lacks executable mode | 4 | `invalid_executable_mode` | Restore executable mode in the package, then run `acr realize --dry-run` | [Native adapters](adapters.md#native-adapters) |
| A native event spelling is invalid for its adapter | 4 | `invalid_native_event` | Correct the package event, then run `acr realize --dry-run` | [Native adapters](adapters.md#native-adapters) |
| Cursor rule frontmatter cannot be parsed safely | 4 | `malformed_frontmatter` | Correct the source rule frontmatter, then run `acr realize --dry-run` | [Native adapters](adapters.md#native-adapters) |
| `acr check` found a conflict-free plan with unapplied work | 3 | `realization_changes` | Review the plan and run `acr realize` | [Modes](realization.md#modes-and-transaction-boundary) |
| Materialization, rendering, or planning fails without a narrower code | 1 | `realization_failed` | Run `acr realize --dry-run --json`, inspect the cause, and retry | [Realization](realization.md) |
| A remaining dependency cannot be fetched while uninstalling another | 1 | `remaining_packages_unavailable` | Restore repository access, then run `acr uninstall SOURCE` | [CLI removal](cli.md#removing-a-dependency) |

## Commands, publishing, freshness, and transactions

| Symptom | Exit | Error code | Remedy | Owning page |
| --- | --- | --- | --- | --- |
| A requested dependency is absent from `agents.yaml` | 2 | `dependency_not_declared` | Run `acr list`, then retry with a declared source | [Installation policy](cli.md#installation-policy) |
| Dependency resolution or state writing fails | 1 | `dependency_operation_failed` | Run `acr install --dry-run --json`, inspect the cause, and retry | [Dependencies](dependencies.md#resolution-policy) |
| An interactive rollback question is declined | 2 | `downgrade_cancelled` | Run `acr install SOURCE@VERSION --hold` or `acr install SOURCE@VERSION --pin` | [Rollback holds](cli.md#rollback-holds) |
| A rollback needs an explicit temporary/permanent choice | 2 | `downgrade_choice_required` | Run `acr install SOURCE@VERSION --hold` or `acr install SOURCE@VERSION --pin` | [Rollback holds](cli.md#rollback-holds) |
| Git cannot inspect the package repository | 1 | `git_access_failed` | Install Git or repair the repository, then run `acr publish --dry-run` | [Publishing](publishing.md#publish-a-package) |
| JSON result encoding fails | 1 | `json_encoding_failed` | Retry the same `acr` command without `--json`, then report the failure | [Output contract](cli.md#output-contract) |
| No agent was selected during setup | 2 | `no_agent_selected` | Run `acr init --agent claude-code` or select `codex` or `cursor` | [Setup policy](cli.md#setup-policy) |
| A publishable tag is absent at `HEAD` | 1 | `no_publishable_tag` | Create the version tag, then run `acr publish --dry-run` | [Publishing](publishing.md#publish-a-package) |
| More than one local tag points at `HEAD` | 1 | `ambiguous_tag` | Remove the unintended tag, then run `acr publish --dry-run` | [Publishing](publishing.md#publish-a-package) |
| A command reached an unavailable application seam | 1 | `not_implemented` | Upgrade `acr`, then retry the original `acr` command | [Commands](cli.md#commands) |
| An uncategorized command failure reaches the CLI boundary | 1 | `operation_failed` | Retry the original `acr` command, then report the failure with its cause | [Output contract](cli.md#output-contract) |
| Stdout rejects a text or JSON write | 1 | `output_failed` | Make stdout writable, then retry the original `acr` command | [Output contract](cli.md#output-contract) |
| Publication fails without a narrower refusal | 1 | `publish_failed` | Run `acr publish --dry-run --json`, inspect the cause, and retry | [Publishing](publishing.md#publish-a-package) |
| A visible release already owns the tag | 1 | `release_already_exists` | Bump the manifest version, commit and tag it, then run `acr publish` | [Immutable failure handling](publishing.md#immutable-failure-handling) |
| Draft creation, asset verification, or publication fails | 1 | `release_upload_failed` | Inspect the named draft, then run `acr publish` again | [Immutable failure handling](publishing.md#immutable-failure-handling) |
| A same-tag draft cannot prove ACR ownership | 1 | `foreign_draft_release` | Inspect or remove the draft manually, then run `acr publish` | [Immutable failure handling](publishing.md#immutable-failure-handling) |
| The remote tag resolves to another commit | 1 | `tag_commit_mismatch` | Restore the tag or make a new version, then run `acr publish --dry-run` | [Immutable failure handling](publishing.md#immutable-failure-handling) |
| The local version tag is absent on GitHub | 1 | `tag_not_pushed` | Push the tag, then run `acr publish --dry-run` | [Immutable failure handling](publishing.md#immutable-failure-handling) |
| The tag and manifest versions differ | 1 | `tag_version_mismatch` | Correct either value, then run `acr publish --dry-run` | [Publishing](publishing.md#publish-a-package) |
| A tagged manifest path cannot be read | 1 | `unpublishable_path` | Commit the declared file, then run `acr publish --dry-run` | [Published files](package-manifest.md#published-files) |
| Package realization dirties the publication worktree | 1 | `dirty_worktree` | Commit or remove local changes, then run `acr publish --dry-run` | [Publishing](publishing.md#publish-a-package) |
| Setup is cancelled before every required answer is submitted | 2 | `setup_cancelled` | Run `acr init --agent claude-code --freshness outdated` with explicit choices | [Setup policy](cli.md#setup-policy) |
| Setup detection or state writing fails | 1 | `setup_failed` | Run `acr init --dry-run --json`, inspect the cause, and retry | [Setup policy](cli.md#setup-policy) |
| A command, flag, or argument is invalid | 2 | `usage` | Run `acr help COMMAND`, correct the invocation, and retry | [Commands](cli.md#commands) |
| A direct command finds a complete recovery journal | 1 | `pending_transaction` | Retry the original mutating `acr` command to recover it | [Transaction boundary](realization.md#modes-and-transaction-boundary) |
| Recovery sees bytes matching neither journal state | 1 | `recovery_conflict` | Reconcile the named file, then retry the original `acr` command | [Transaction boundary](realization.md#modes-and-transaction-boundary) |
| Another process owns the project transaction claim | 1 | `transaction_busy` | Wait for that process, then retry the original `acr` command | [Transaction boundary](realization.md#modes-and-transaction-boundary) |
| The filesystem cannot provide the required advisory lock | 1 | `transaction_lock_unavailable` | Move the project to a locking filesystem, then retry the original `acr` command | [Transaction boundary](realization.md#modes-and-transaction-boundary) |
| The recovery journal schema is unsupported | 1 | `unsupported_journal_version` | Upgrade `acr`, then retry the original `acr` command | [Transaction boundary](realization.md#modes-and-transaction-boundary) |
| A direct freshness check cannot authenticate | 1 | `freshness_auth` | Run `gh auth login`, then run `acr freshness run` again | [Session-start freshness](cli.md#session-start-freshness) |
| Freshness realization hits an ownership refusal | 4 | `freshness_conflict` | Resolve the named target, then run `acr freshness run --policy install` | [Session-start freshness](cli.md#session-start-freshness) |
| The freshness project lock cannot be released | 1 | `freshness_lock_release_failed` | Inspect the state path, then run `acr freshness run` again | [Session-start freshness](cli.md#session-start-freshness) |
| A direct freshness check cannot reach GitHub | 1 | `freshness_offline` | Restore network access, then run `acr freshness run` again | [Session-start freshness](cli.md#session-start-freshness) |
| The machine-local freshness state cannot be written | 1 | `freshness_state_unwritable` | Make `ACR_STATE_HOME` writable, then run `acr freshness run` again | [Session-start freshness](cli.md#session-start-freshness) |
| Freshness reconciliation or project-state loading fails | 1; notice at exit 0 for policy `none` | `freshness_update_failed` | Repair project state, then run `acr freshness run --policy none` or the intended policy | [Session-start freshness](cli.md#session-start-freshness) |
| A direct `vendor:` install, targeted update, or resume is attempted | 2 | `vendor_source_read_only` | Run `acr migrate tessl --map FROM=SOURCE` or `acr uninstall vendor:workspace/package` | [Vendored packages](migration.md#vendored-packages) |

## Notices

Notices do not authorize extra writes. They describe an exit-0 result or accompany the command's normal result. `freshness_update_failed` is indexed above: direct freshness failures exit nonzero, while `--policy none` can carry the same code as a fail-open notice.

| Code | Meaning |
| --- | --- |
| `ambiguous` | Finalization retained Tessl evidence whose ownership or normalized meaning is ambiguous. |
| `dependency_hold_resumable` | A stable release newer than the rollback barrier exists; only `acr resume SOURCE` advances it. |
| `duplicate-effect` | A Tessl hook and ACR hook have the same event and may both run during coexistence. |
| `freshness_busy` | Another freshness operation owns the machine-local lock; this attempt wrote no project state. |
| `freshness_outdated` | At least one eligible `latest` dependency has a newer stable release. |
| `gitignored_state` | An ignore rule covers named ACR state; the notice includes its `<file>:<line>` evidence. |
| `lossy` | Producer or consumer normalization has information with no v1 ACR field. |
| `no-version-control` | Finalization tracking checks do not apply outside a Git worktree. |
| `restart_required` | Install-mode freshness changed native files for the named agents. |
| `shared_file_requires_commit` | A generated target became authoritative shared source and should be committed. |
| `stale_transaction_staging` | An incomplete staging directory exists; only a mutating command under the project claim removes it. |
| `tessl_not_installed` | No Tessl installation remains; migration has nothing to change. |
| `uncovered-agent` | Tessl output exists for an agent outside ACR's current adapter coverage. |
| `unsupported` | Finalization retained a Tessl artifact or target without a supported v1 representation. |
