# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr version [--json]` | Report the release version and source commit when known; `--version` and `-v` are aliases | Available |
| `acr init` | Initialize project agent and freshness selections | Available |
| `acr install [SOURCE[@VERSION]] [--hold \| --pin]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Resolution available; realization in #7 |
| `acr realize [--agent NAME] [--dry-run]` | Verify and reapply locked packages into selected native layouts | Available |
| `acr list` | List declared and resolved dependencies | Available |
| `acr outdated` | Check `latest` dependencies without modifying project state | Available |
| `acr freshness run [--policy POLICY]` | Run the throttled session-start policy for this project | Available |
| `acr update [SOURCE]` | Update one dependency or all eligible dependencies | Resolution available; realization in #7 |
| `acr resume SOURCE` | Clear a rollback hold and resume `latest` | Available |
| `acr uninstall SOURCE` | Remove a dependency and its owned artifacts | Available |
| `acr check [--agent NAME]` | Report native-layout drift without applying changes | Available |
| `acr publish [PATH] [--dry-run]` | Validate and publish an immutable package | Available |
| `acr migrate tessl [--mapping-file PATH] [--map FROM=SOURCE[@REQUESTED]] [--vendor-unmapped] [--finalize] [--dry-run]` | Migrate a Tessl consumer, preserve unmapped packages locally, or remove Tessl after convergence | Available |
| `acr migrate tessl-plugin [PATH]` | Convert a Tessl plugin package to `agent-plugin.yaml` | Producer conversion in #11 |

Every domain command supports `--help`, `--json`, and `--project PATH`. Mutating commands support `--dry-run`. `install` accepts the mutually exclusive `--hold` and `--pin` rollback choices described under [rollback holds](#rollback-holds). `init`, `install`, and `migrate tessl` support `--non-interactive`. `init`, `install`, `realize`, and `check` accept repeated `--agent claude-code|codex|cursor`; without flags, `realize` and `check` use the sorted `agents` selection in `agents.yaml`. `uninstall` accepts no `--agent`. `acr migrate tessl-plugin` takes the plugin package root as a positional PATH, the same way `acr publish [PATH]` does, and accepts `--repository URL` and `--accept-agent-widening`. The standalone `version` command supports `--json` but has no project state.

Producer conversion is documented in the [producer migration reference](migration-producer.md).

## Realization

`acr realize` downloads each immutable locked commit, revalidates package identity, version, and content hash, renders the selected native layouts, and applies the resulting plan transactionally. It updates the ownership ledger under `realization` in `.agents/registry.lock` only after all file operations succeed. `--dry-run` returns the plan without writing files or the ledger.

`acr check` runs the same materialization, rendering, preservation, and planning path in read-only mode. It exits `0` when current, `3` when a conflict-free plan has unapplied changes, and `4` for a refusal listed in the [exit-4 table](#exit-4-refusal-codes). Adapter validation completes before the transactional engine is invoked.

An explicit `--agent` list overrides the persisted selection for that invocation and does not rewrite `agents.yaml`. A project with neither flags nor persisted agents fails with guidance to select an adapter.

`--agent` is a pure subset override: the agents it omits keep their realized outputs and their ownership entries, and `acr check --agent` reports only the selected agents' drift. Removing an agent from `agents.yaml` is the persisted change, and the next `acr realize` without flags removes what that agent left behind. A single ownership entry owned by both a selected and an omitted agent cannot be scoped either way, so it exits `4` with `realization_conflict` and names `--agent`.

## Removing a dependency

`acr uninstall SOURCE` drops the declaration, its rollback hold, and its lock row, then runs the ordinary realization pass over the pruned state. A generated-only target the remaining packages no longer want is deleted; a target shared with another package or with operator content keeps everything else and loses only the removed package's entries, bound to the observed hash. Unmanaged content, other packages' outputs, `agents`, `freshness`, unknown `agents.yaml` fields, and the machine-local freshness timer are never touched.

For a `vendor:<workspace>/<package>` source, uninstall also plans removal of `.agents/vendor/<workspace>/<package>`. The vendor tree is ACR-owned, so hand edits do not block its removal. The tree is removed through a second recovery journal only after the prune-and-realize transaction commits; shared vendor parents remain until empty.

After the expected `agents.yaml` and `.agents/registry.lock` state updates, `.git/info/exclude` is the sole path outside the previous ownership ledger an uninstall may write. The `# BEGIN ACR GENERATED OUTPUTS` block is ACR's own local metadata, not a package output, so removing a generated-only target also prunes that target's pattern from the block. The file is never removed, the block is never removed while another generated-only target still needs it, and no byte outside the block changes.

Removal keys on the declaration, never on a ledger source match. The session-start hook is contributed under this repository's own source, so a project that also declares `github:jbaruch/agentic-context-registry` keeps its hook; `--freshness none` remains the only way to remove it.

Uninstall accepts no `--agent`. It realizes across the union of the agents `agents.yaml` selects and every agent the ownership ledger records, so a narrowed selection cannot leave another agent's outputs carrying entries for a package that is gone.

Re-rendering the packages that remain needs their sources, so only the last dependency uninstalls fully offline. Materialization runs before any file is planned, so an unreachable source exits `1` with `remaining_packages_unavailable` having written nothing. `--dry-run` prints the same plan and writes nothing, including no state write. A second uninstall of the same source exits `2` with `dependency_not_declared` and names `acr list`.

| Code | Meaning |
| --- | --- |
| `dependency_not_declared` | `SOURCE` names no declared dependency; also `acr resume` and `acr update` |
| `remaining_packages_unavailable` | A package that survives the uninstall could not be re-rendered |

Removal ownership conflicts use `realization_conflict` as listed in the [exit-4 table](#exit-4-refusal-codes).

## Tessl migration

`acr migrate tessl --dry-run` reads `tessl.json`, installed plugin and tile manifests, `.tessl/RULES.md`, and native Tessl outputs, resolves explicitly mapped ACR packages, and prints a schemaVersion 1 coexistence plan. Omitting `--dry-run` writes ACR-owned native output plus `agents.yaml` and `.agents/registry.lock` in one journaled transaction. It does not edit or remove Tessl-owned bytes.

Mappings are selected by repeatable `--map`, then `--mapping-file`, then a package manifest's repository field. A package name is never guessed as a repository. A Tessl package version is resolved to exactly one matching GitHub release tag; an explicit `@REQUESTED` mapping bypasses that conversion.

The report classifies tool-owned, frozen Tessl-owned, and preserved unmanaged migration surfaces; compares effective rule, skill, and hook behavior; and lists finalization blockers. `--vendor-unmapped` copies packages without repository evidence into `.agents/vendor` and records `vendor:` locks. `--finalize` is a separate transaction: it exits `4` until every equivalence and recoverability gate is clear, then removes only positively identified Tessl output. See [`docs/migration.md`](migration.md).

Stable migration outcomes include:

| Code | Exit | Meaning |
| --- | --- | --- |
| `tessl_not_installed` | `0` | Notice that `tessl.json` remains but the installed `.tessl` tree is absent |
| `unmapped_package` | `1` | A package needs an explicit mapping or `--vendor-unmapped` |
| `no_artifacts` | `1` | A synthesized vendor manifest declares no rule, skill, script, or hook |
| `duplicate_artifact_id` | `1` | A synthesized vendor manifest maps one ID to multiple artifact paths |
| `vendor_collision` | `4` | Existing or superseded vendor content differs from the verified tree |
| `finalization_conflict` | `4` | Tessl-owned content changed after finalization planning |

## Installation policy

An unversioned source such as `github:owner/plugin` requests the `latest` stable release. An explicit suffix such as `@v1.2.3` or `@COMMIT_SHA` requests a fixed dependency. Running `acr install` without a source reconciles dependencies already declared in `agents.yaml`, including refreshing declarations whose requested policy is `latest`.

The resolver records the requested policy separately from the immutable release, commit, and content hash selected for the lockfile. A successful non-dry-run install also persists the selected `--freshness` value; when no value has been stored or supplied, it persists `outdated`.

## Rollback holds

Installing an explicit reference that does not move a `latest` dependency forward is a rollback, and it needs a choice. Without one, `acr install SOURCE@REF` exits `2` with the code `downgrade_choice_required` and names both flags:

1. `--hold` keeps `requested: latest` and records a temporary rollback: the known-good pin plus the rejected release, which becomes the resume barrier.
2. `--pin` replaces `latest` with a permanent pin and removes any hold. This is the only sanctioned hold-to-pin conversion.

On a terminal, passing neither asks the same choice as a three-option question with cancel as its third option and no default; see [setup policy](#setup-policy). Everywhere else — `--json`, `--non-interactive`, a non-terminal stdin — passing neither is cancel's non-interactive form. The flags are mutually exclusive and require an explicit `SOURCE@VERSION`.

While a hold stands, `acr install`, `acr update`, and the session-start `install` policy all preserve the held release and never reinstall the rejected one. Both flags then accept only a reference proven not to move the held resolution forward: the reference the lock already resolves, or a semver-older tag. A newer or unorderable reference is refused and names `acr resume`, because a held dependency moves forward through no other path.

`acr resume SOURCE` is the only command that resumes `latest`: it deletes the hold from both files, resolves `latest` again, and writes through the same transaction as install. `--dry-run` reports the resolution it would write without touching any file. `acr install SOURCE@REF --pin` also ends a hold, by leaving `latest` behind for a permanent pin rather than returning to it.

`acr list` marks a held row as `SOURCE@latest [held PIN, barrier REJECTED] -> COMMIT`. `acr outdated` classifies every row as `update`, `held`, or `beyond-barrier`; only `beyond-barrier` rows carry a `resumeCommand`. A `held` steady state is reported when you run the command and stays silent at session start, where a `dependency_hold_resumable` notice appears only once a stable release newer than the barrier exists.

Rollback semantics and the `agents.yaml` shape are documented in [dependency declarations](dependencies.md#rollback-holds).

## Publishing

`acr publish [PATH]` validates `agent-plugin.yaml`, requires a clean Git worktree with exactly one version-matching tag at `HEAD`, builds release assets from that tag's committed blobs, realizes the resulting archive through every supported adapter, and verifies the remote tag before creating a GitHub Release. `PATH` defaults to the current directory.

`--dry-run` executes validation, archive construction, adapter realization, and the remote immutability probes without creating, deleting, or uploading a release. `--json` returns the planned tag, commit, content hash, and three asset names in the normal success envelope.

Publisher refusals use exit code `1`. Stable error codes include:

| Code | Meaning |
| --- | --- |
| `no_publishable_tag` | `HEAD` has no tag |
| `dirty_worktree` | The Git worktree has uncommitted or untracked changes |
| `git_access_failed` | Git is unavailable or cannot inspect the package repository |
| `unpublishable_path` | A manifest-declared file cannot be read from the tagged Git tree |
| `ambiguous_tag` | More than one tag points at `HEAD` |
| `tag_version_mismatch` | The tag does not equal the manifest version with one optional leading `v` |
| `adapter_realization_failed` | The archive does not realize idempotently through every supported adapter |
| `release_already_exists` | A visible release already owns the immutable version |
| `tag_commit_mismatch` | The pushed tag points at a different commit |
| `tag_not_pushed` | GitHub does not have the local version tag |
| `foreign_draft_release` | A same-tag draft contains an asset not owned by ACR |
| `release_upload_failed` | Draft creation, upload verification, or publication failed |

See [Publishing packages](publishing.md) for archive normalization, release assets, draft recovery, and the reusable workflow.

## Setup policy

`acr init`, and the first `acr install SOURCE` of a project that has no `agents.yaml`, select the agents to realize for and one session-start freshness policy:

1. `outdated` checks for updates and is the default.
2. `install` reconciles dependencies declared as `latest`.
3. `none` installs no freshness hook.

On a project with no `agents.yaml`, the detected agents are pre-selected. On a project that already has one, the stored `agents` selection is pre-selected and detection only contributes candidates: a detected agent that is not stored is offered unselected, and a stored agent detection misses stays selected. Accepting the selection unchanged reports `changed:false` and writes nothing. Absence of `agents.yaml`, not an empty `agents` list, triggers the first-install questions, so a project that deliberately selected nothing is never re-asked.

Repeated `--agent NAME` flags win outright: they suppress both detection and the question, and they replace a stored selection, which is the only way to narrow one. `--freshness outdated|install|none` does the same for the policy. Selecting no agent is refused with exit `2` and the code `no_agent_selected` in every mode, because an `agents.yaml` that selects no adapter cannot realize anything. A configured project that cannot prompt returns its stored selection before detection runs at all, so a malformed detected agent file cannot fail an `acr init --non-interactive` that has nothing to decide.

Declining a question exits `2` with the code `setup_cancelled` and writes nothing. Input that ends mid-question is declining: end of input is read before the answer line is parsed, so a preselected agent set or the preselected `outdated` never stands in for an answer nobody gave, and a partial line without its newline is not a submitted one.

`acr install SOURCE@VERSION` that rolls a `latest` dependency backwards asks whether to record a `--hold` or a `--pin`. The question costs nothing: the install refuses before it resolves anything and before it writes either state file. Declining — an explicit cancel, an empty answer, end of input, or three unparsable answers — exits `2` with the code `downgrade_cancelled` and writes nothing.

`--non-interactive`, `--json`, and a non-terminal stdin all mean no question and the typed refusal instead. Only a terminal is interactive: a pipe, a regular file, `/dev/null`, and a descriptor closed before the process starts — which the Go runtime reopens onto `/dev/null` — are each a non-terminal stdin. Questions are written to stderr and answers are read from stdin, so stdout carries only program output in either format.

## Session-start freshness

For `outdated` and `install`, realization adds one ACR-owned `session-start` hook to every selected native adapter. `none` contributes no hook and removes only the previously owned ACR hook on the next realization. User hooks and Codex `hooks.state` trust data are preserved.

The generated wrapper runs:

```sh
acr freshness run --project PROJECT --policy outdated|install
```

The wrapper never prompts and always exits `0`, so a missing binary, network failure, update failure, or ownership conflict cannot block agent startup. A throttled or no-change run emits nothing. When there is a status, the wrapper injects one native session-start context payload beginning `Session-start status — `; Claude Code and Codex receive `hookSpecificOutput.additionalContext`, while Cursor receives `additional_context`. Set `ACR_BIN` when the executable is not discoverable as `acr`.

A direct `acr freshness run` without `--policy` uses the `freshness` value stored in `agents.yaml`. An explicit `--policy` overrides the stored value for that invocation.

`outdated` is read-only: it reports newer stable releases only for dependencies declared as `latest`. `install` first reconciles those `latest` dependencies, then applies the normal transactional realization path. Explicit tag and commit pins are not advanced by either policy. If hook configuration or realized package content changes, a `restart_required` notice names the affected agents. JSON results keep every realized agent in `agents` and place only the affected subset in `restartAgents`.

Remote checks are limited to one attempt per project and policy in each 24-hour window. The machine-local record is stored outside the project at `${ACR_STATE_HOME:-<user-cache>/acr}/freshness/<project-key>.json`; the key uses the canonical project path, so different path spellings of one checkout share a timer. A policy change runs immediately. Missing, corrupt, or unsupported state is treated as no prior attempt and rewritten after the run. A future `lastCheckedAt` is not throttled; the next check runs and rewrites it with the current attempt time.

The direct `acr freshness run` command preserves the normal process exit contract: operational, network, authentication, update, state-write, and lock-release failures exit `1`; preservation or ownership conflicts use the [exit-4 contract](#exit-4-refusal-codes); lock contention exits `0`. `--policy none` runs no check and exits `0` even when `agents.yaml` cannot be read, and reports that unreadable project state as a `freshness_update_failed` notice rather than staying silent. The generated wrapper converts all of these outcomes to `0`.

## Output contract

Human-readable results are written to stdout. Structured notices are written one per line to stderr. JSON mode echoes result notices in `result.notices`; if an application error replaces the result, the runner suppresses separate notice lines and writes one error object to stderr. Progress and diagnostics never contaminate the JSON document. Most commands write one success object to stdout or one error object to stderr. `acr freshness run --json` always writes its completed attempt as one result envelope on stdout. Its `ok` field matches the process exit code, while stderr notices describe the fail-open domain outcome.

Success envelope:

```json
{"ok":true,"command":"list","result":{}}
```

Error envelope:

```json
{"ok":false,"command":"install","error":{"code":"operation_failed","message":"..."}}
```

Producer conversion refusals include `field` on the error object when the named code points at a Tessl field. Errors may also carry a `remedy` field; for example, finalization of an untracked manifest returns `"remedy":"git add tessl.json && git commit"`.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Command, help, or version completed successfully |
| `1` | Operational failure, including producer conversion refusals (`unknown_field`, `unmapped_field`, `agent_widening`, `ambiguous_manifest`, `manifest_conflict`, `unpublishable_content`, and #4 validation codes) |
| `2` | Invalid command, flag, or argument |
| `3` | `check` found unapplied changes |
| `4` | A refusal listed below |

### Exit-4 refusal codes

| Cause | Error code | Commands |
| --- | --- | --- |
| Managed/unmanaged target conflict | `realization_conflict` | `acr migrate tessl`, `acr realize`, `acr check`, `acr uninstall`, `acr freshness run` |
| Preservation conflict | `realization_conflict` | `acr migrate tessl`, `acr realize`, `acr check`, `acr uninstall`, `acr freshness run` |
| Tessl finalization gate is blocked | `finalization_blocked` | `acr migrate tessl --finalize` |
| Tessl-owned content changed during finalization | `finalization_conflict` | `acr migrate tessl --finalize` |
| Verified vendor content conflicts with the destination | `vendor_collision` | `acr migrate tessl` |
| A realization plan targets a Tessl-owned path | `tessl_owned_target` | `acr migrate tessl`, `acr realize`, `acr check` |

## Platforms

Tagged releases publish `acr-darwin-amd64.tar.gz`, `acr-darwin-arm64.tar.gz`, `acr-linux-amd64.tar.gz`, and `acr-linux-arm64.tar.gz`. Each candidate runs on its native CI runner before publication. Homebrew installation is tested on macOS and Linux. See [Installing acr](install.md) for Homebrew, verified direct downloads, `go install`, and the macOS Gatekeeper validation procedure. Native Windows is outside the MVP.
