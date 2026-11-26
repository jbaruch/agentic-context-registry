# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr version [--json]` | Report the release version and source commit when known; `--version` and `-v` are aliases | Available |
| `acr help [COMMAND]` | Show root help or the exact usage and options for one command | Available |
| `acr init [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]` | Initialize project agent and freshness selections | Available |
| `acr install [SOURCE[@VERSION] \| PATH] [--hold \| --pin \| --if-missing] [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Available |
| `acr realize [--agent NAME] [--dry-run]` | Verify and reapply locked packages into selected native layouts | Available |
| `acr list` | List declared and resolved dependencies | Available |
| `acr outdated` | Check `latest` dependencies without modifying project state | Available |
| `acr freshness run [--policy POLICY]` | Run the throttled session-start policy for this project | Available |
| `acr update [SOURCE] [--dry-run]` | Update one dependency or all eligible dependencies | Available |
| `acr resume SOURCE [--dry-run]` | Clear a rollback hold and resume `latest` | Available |
| `acr uninstall SOURCE [--dry-run]` | Remove a dependency and its owned artifacts | Available |
| `acr check [--agent NAME]` | Report native-layout drift without applying changes | Available |
| `acr validate [PATH]` | Validate the authored manifest and complete distribution file inventory locally | Available |
| `acr publish [PATH] [--dry-run]` | Validate and publish an immutable package | Available |
| `acr migrate tessl [--mapping-file PATH] [--map FROM=SOURCE[@REQUESTED]] [--vendor-unmapped] [--finalize] [--accept-reviewed-changes TOKEN] [--non-interactive] [--dry-run]` | Migrate a Tessl consumer, preserve unmapped packages locally, or remove Tessl after convergence | Available |
| `acr migrate tessl-plugin [PATH] [--dry-run] [--repository URL] [--acr-only [--package-version SEMVER]]` | Convert a Tessl plugin package to `agent-plugin.yaml` | Available |

Every domain command supports `--help`, `--json`, and `--project PATH`. Except for help and usage handling, the selected project must exist and permit directory read and search access before the command runs, even with an absolute package `PATH`. Mutating commands support `--dry-run`. `install` accepts the mutually exclusive `--hold` and `--pin` rollback choices described under [rollback holds](#rollback-holds). `init`, `install`, and `migrate tessl` support `--non-interactive`, which selects the default answer and never stands in for an approval. `migrate tessl --finalize` accepts `--accept-reviewed-changes TOKEN`. `init`, `install`, `realize`, and `check` accept repeated `--agent claude-code|codex|cursor|antigravity`; without flags, `realize` and `check` use the sorted `agents` selection in `agents.yaml`. `uninstall` accepts no `--agent`. `acr migrate tessl-plugin` takes the plugin package root as a positional PATH, the same way `acr publish [PATH]` does, and accepts `--repository URL`. Its `--acr-only` mode requires an explicit target repository and accepts `--package-version SEMVER`; both new flags are producer-only, and the version override requires clean mode. The standalone `version` command supports `--json` but has no project state.

Producer conversion is documented in the [producer migration reference](migration-producer.md).

## Executable examples

Every console transcript in this documentation runs in an isolated fixture during `go test`. These examples cover local, remote-backed, read-only, and dry-run paths without network access.

```console
$ acr help version
# fixture: bare
# exit: 0
Usage:
  acr version [--json]
```

```console
$ acr init --agent codex --freshness none --non-interactive --dry-run
# fixture: bare
# exit: 0
Would select codex with freshness none in agents.yaml; rerun without --dry-run to write it.
```

```console
$ acr install github:example/alpha --agent codex --freshness none --non-interactive --dry-run
# fixture: bare
# exit: 0
install would update dependency state; rerun without --dry-run to write agents.yaml and .agents/registry.lock.
```

```console
$ acr list
# fixture: bare
# exit: 0
No dependencies declared.
```

```console
$ acr outdated
# fixture: github-installed
# exit: 0
All latest dependencies are current.
```

`outdated` confirms currency only when a latest lookup ran. A project with no
declarations, and one whose declarations are all pinned or vendored, both say
so instead:

```console
$ acr outdated
# fixture: bare
# exit: 0
No dependencies declared; nothing to check.
```

```console
$ acr update github:example/alpha --dry-run
# fixture: github-installed
# exit: 0
Dependency state is already current.
github:example/alpha v1.0.0 unchanged
```

`update` names the version it locked for each dependency it reports: `SOURCE OLD -> NEW` when the lock row moved, or `SOURCE VERSION unchanged` when it did not. A dependency that was declared but not locked reads `SOURCE none -> NEW`. The version is the release tag, a pinned commit, a vendored package version, or `local PATH`. Without `SOURCE`, every locked dependency is reported; with `SOURCE`, that dependency and any other row that moved. `--json` carries the same rows in `result.versions`, each with `source`, `from`, `to`, and `changed`; `from` is omitted when there was no lock.

```console
$ acr resume github:example/alpha --dry-run
# fixture: github-held
# exit: 0
resume would update dependency state; rerun without --dry-run to write agents.yaml and .agents/registry.lock.
Would resume latest for github:example/alpha and retire its rollback barrier.
```

```console
$ acr check
# fixture: initialized
# exit: 0
Realization is current for codex.
```

A current checkout passes `check` even when its local receipt is missing, stale, invalid or oversized. The receipt and ignore rule are maintained by successful `acr realize`, the realization phase of `acr uninstall`, non-finalizing `acr migrate tessl` coexistence apply, and install-mode freshness. `acr migrate tessl --finalize` leaves them unchanged; a subsequent realization may refresh them. Check, dry-run and none/outdated freshness do not maintain them. The ignore file converges when the canonical receipt rule `/.acr-applied.json` occurs after every unescaped negation. A later unrelated negation may cause one appended canonical rule. Fully converged auxiliary files remain untouched on subsequent runs. Auxiliary-only maintenance does not count as a realization change.

Receipt reads are bounded at 32 MiB. Check and dry-run treat an oversized receipt as unusable evidence and compare against the committed ledger. Apply refuses an oversized receipt before journaling: back up/remove the named local receipt and retry `acr realize`, preserving `.agents/registry.lock`. Mode-000 receipt or ignore files are refused before opening; deliberately restore readable permissions and retry. When the native plan has no changes, apply renders and compares the receipt before journaling. Plans with native changes render the receipt after output renames bind its final metadata.

```console
$ acr realize --dry-run
# fixture: initialized
# exit: 0
Realization would apply 0 change(s) for codex.
```

```console
$ acr uninstall github:example/alpha --dry-run
# fixture: github-installed
# exit: 0
Would remove github:example/alpha@v1.0.0; deleted 0 target(s) and spliced 0 shared target(s) for codex.
```

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

`acr migrate tessl --dry-run` reads `tessl.json`, installed plugin and tile manifests, `.tessl/RULES.md`, and native Tessl outputs, resolves explicitly mapped ACR packages, and prints a schemaVersion 3 coexistence plan. Omitting `--dry-run` writes ACR-owned native output plus `agents.yaml` and `.agents/registry.lock` in one journaled transaction. It does not edit or remove Tessl-owned bytes.

Mappings are selected by repeatable `--map`, then `--mapping-file`, then a package manifest's repository field. A package name is never guessed as a repository. A Tessl package version is resolved to exactly one matching GitHub release tag; an explicit `@REQUESTED` mapping bypasses that conversion.

The report classifies tool-owned, frozen Tessl-owned, and preserved unmanaged migration surfaces; compares effective rule, skill, and hook behavior; and lists finalization blockers. `--vendor-unmapped` copies packages without repository evidence into `.agents/vendor` and records `vendor:` locks. `--finalize` is a separate transaction: it exits `4` until every equivalence and recoverability gate is clear, then removes only positively identified Tessl output, including the shared `.agents/skills/tessl__*` links and the canonical `tessl` MCP server entry. A blocked run returns the full report alongside the error, with `blockers[]` naming each gate, its path, and its remedy.

An intentionally changed replacement package is finalized through explicit acceptance, never through a force switch. A `--finalize --dry-run` preview lists every reviewable difference under `acceptance`, with a token bound to the installed Tessl package's own content, the replacement's source, requested ref, resolved commit and resolved content, and the exact differences. Re-running `--finalize --accept-reviewed-changes TOKEN` accepts those differences and nothing else. `--non-interactive` is not acceptance. The token binds that evidence, not the project: a change to the installed package, the resolution, or the differences invalidates it and the run refuses with an `acceptance-stale` blocker, while two projects holding identical evidence share one token. Rewriting a rule `description`'s text and reordering activation globs are the two edits that deliberately do not invalidate a token; `docs/migration.md` lists what does. Accepted differences stay listed in `effectiveDiffs` and are reported again in `acceptedChanges`; they are never relabelled as identical. Acceptance reaches only an artifact's own difference and lossy report, and only for an artifact ACR classified `migratable`: an `unsupported` artifact raises its own blocker, and ambiguity, uncovered agents, unproven ownership, a missing replacement, symlink escapes, untracked state, and transaction recovery all keep blocking. See [`docs/migration.md`](migration.md).

Stable migration outcomes include:

| Code | Exit | Meaning |
| --- | --- | --- |
| `tessl_not_installed` | `0` | Notice that `tessl.json` remains but the installed `.tessl` tree is absent |
| `unmapped_package` | `1` | A package needs an explicit mapping or `--vendor-unmapped` |
| `root_skill_requires_manual_migration` | `1` | A package-root skill needs a source-package fix or manual project installation; other packages can migrate. See [Package mappings](migration.md#package-mappings) |
| `no_artifacts` | `1` | A synthesized vendor manifest declares no rule, skill, script, or hook |
| `duplicate_artifact_id` | `1` | A synthesized vendor manifest maps one ID to multiple artifact paths |
| `vendor_collision` | `4` | Existing or superseded vendor content differs from the verified tree |
| `finalization_conflict` | `4` | Tessl-owned content changed after finalization planning |

## Installation policy

An unversioned source such as `github:owner/plugin` requests the `latest` stable release. An explicit suffix such as `@v1.2.3` or `@COMMIT_SHA` requests a fixed dependency. Running `acr install` without a source reconciles dependencies already declared in `agents.yaml`, including refreshing declarations whose requested policy is `latest`.

An invalid dependency source exits `2`. The diagnostic names `github:owner/repository` and says to review the command usage. It does not ask you to file an issue. A bare name keeps the `./NAME` local-directory hint. `acr update SOURCE`, `acr resume SOURCE`, and `acr migrate tessl` with `--map` or `--mapping-file` classify that same invalid source as exit `2`. `acr uninstall SOURCE` does too, and because uninstall also accepts `vendor:workspace/package` its diagnostic names both forms. A valid source that fails while resolving or writing state stays exit `1`. A canonical `vendor:` source on install, update, or resume stays the read-only vendored-dependency refusal.

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

`acr validate [PATH]` checks the authored manifest, artifact paths, skill support files and distribution inventory without credentials, network calls or writes. It needs no Git: with no repository around `PATH`, a bare repository, or no `git` executable it lists the whole directory. When `PATH` is inside a Git work tree it runs the read-only `git --no-replace-objects rev-parse --is-inside-work-tree` and `git --no-replace-objects --literal-pathspecs ls-files -z --cached --others --exclude-standard -- <declaration>`, and any other Git failure is reported as `git_access_failed`. This way files Git ignores, such as `__pycache__/*.pyc`, are left out of the inventory the way `acr publish` leaves them out; see [Published files](package-manifest.md#published-files). `PATH` defaults to the current directory. JSON reports `valid`, `name`, `version` and the sorted `files`; invalid input exits `1`. Validation does not supply a review score or perform publication.

`acr publish [PATH]` validates `agent-plugin.yaml`, requires a clean Git worktree with exactly one version-matching tag at `HEAD`, selects files from the resolved tagged commit's tree and builds release assets from blob object IDs, realizes the resulting archive through every supported adapter, and verifies the remote tag before creating a GitHub Release. Omitted `PATH` or `.` selects `--project` (the current directory by default); a relative `PATH` resolves under that project. A relative `--project` resolves from the current directory. After the project access check, an absolute `PATH` selects the package independently of the project. Dry-run and publication use the same selection. Declarations must match Git's selected path bytes exactly; local validation may succeed through filesystem aliases while publication refuses a mismatch. Committed manifest bytes stay unchanged. The shared Git runner disables replacement objects for validation and publication, including identity, tree selection and blob reads.

`--dry-run` executes validation, archive construction, adapter realization, and the remote immutability probes without creating, deleting, or uploading a release. `--json` returns the planned tag, commit, content hash, and three asset names in the normal success envelope.

Publisher refusals use exit code `1`. Stable error codes include:

| Code | Meaning |
| --- | --- |
| `no_publishable_tag` | `HEAD` has no tag |
| `dirty_worktree` | The Git worktree has uncommitted or untracked changes |
| `git_access_failed` | Git is unavailable, a Git command fails, output is malformed, or a selected blob is missing or corrupt; inspect repository access/integrity |
| `unpublishable_path` | A required declaration is absent from the committed tree, differs byte for byte from Git's selected spelling, or a selected entry has an unsupported mode; spelling mismatches show escaped names and require correcting `agent-plugin.yaml`, committing and retagging |
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

For Antigravity and Codex, run `acr init --agent codex --agent antigravity --freshness outdated`. This sets `schemaVersion: 6` in `agents.yaml`. Add `sharedSkills: true` to that file, then run `acr realize` and `acr check`. Root `AGENTS.md` carries package rules, `.agents/skills/` carries shared skills and `.agent/hooks.json` registers Antigravity hooks. Setup detects root or workspace `GEMINI.md`, hook configs and distinctive customization directories. Setup ignores a `.agents`, `.agent`, `_agents` or `_agent` root that is a symlink or not a directory, and a `hooks.json`, `rules`, `workflows` or `skills` entry inside a real root that is a symlink or the wrong file type, without following either. Evidence at a real path still selects Antigravity. A path ACR cannot read still stops detection, naming the path and the `--agent` remedy. Root `AGENTS.md` or shared `.agents/skills/` alone leaves Antigravity unselected. [Antigravity discovery](https://antigravity.google/docs/customization-discovery)

Antigravity hooks must join their children before returning. Foreground effects can complete after Antigravity acknowledges cancellation, and cleanup cannot undo completed effects. Keep hooks that require zero effects after acknowledgment out of deployment until Antigravity provides a usable cancellation notification and a native replay confirms cleanup. [Antigravity hook runtime](adapters.md#hook-delivery)

Repeated `--agent NAME` flags win outright: they suppress both detection and the question, and they replace a stored selection, which is the only way to narrow one. `--freshness outdated|install|none` does the same for the policy. Selecting no agent is refused with exit `2` and the code `no_agent_selected` in every mode, because an `agents.yaml` that selects no adapter cannot realize anything. A configured project that cannot prompt returns its stored selection before detection runs at all, so a malformed detected agent file cannot fail an `acr init --non-interactive` that has nothing to decide.

Declining a question exits `2` with the code `setup_cancelled` and writes nothing. Input that ends mid-question is declining: end of input is read before the answer line is parsed, so a preselected agent set or the preselected `outdated` never stands in for an answer nobody gave, and a partial line without its newline is not a submitted one.

`acr install SOURCE@VERSION` that rolls a `latest` dependency backwards asks whether to record a `--hold` or a `--pin`. The question costs nothing: the install refuses before it resolves anything and before it writes either state file. Declining — an explicit cancel, an empty answer, end of input, or three unparsable answers — exits `2` with the code `downgrade_cancelled` and writes nothing.

`--non-interactive`, `--json`, and a non-terminal stdin all mean no question and the typed refusal instead. Only a terminal is interactive: a pipe, a regular file, `/dev/null`, and a descriptor closed before the process starts — which the Go runtime reopens onto `/dev/null` — are each a non-terminal stdin. Questions are written to stderr and answers are read from stdin, so stdout carries only program output in either format.

## Session-start freshness

For `outdated` and `install`, realization adds one ACR-owned `session-start` hook to every selected native adapter. `none` contributes no hook and removes only the previously owned ACR hook on the next realization. User hooks and Codex `hooks.state` trust data are preserved.

The generated wrapper runs `acr freshness run --project PROJECT --policy <outdated|install>`.
For a manual no-op invocation:

```console
$ acr freshness run --policy none
# fixture: initialized
# exit: 0
```

JSON recovery guidance preserves an explicit project path:

```console
$ acr freshness run --project PROJECT --policy none --json
# fixture: invalid-state
# exit: 0
{"ok":true,"command":"freshness","result":{"notices":[{"code":"freshness_update_failed","message":"Freshness could not load project state; fix or remove the invalid project file, then run 'acr outdated --project PROJECT' to diagnose the failure."}],"outdated":[],"policy":"none"}}
```

The wrapper never prompts and always exits `0`, so a missing binary, network failure, update failure, or ownership conflict cannot block agent startup. A throttled or no-change run emits nothing. When there is a status, the wrapper injects one native session-start context payload beginning `Session-start status — `; the wrapper emits canonical `hookSpecificOutput.additionalContext`, which the Cursor adapter translates to its native reply. Set `ACR_BIN` when the executable is not discoverable as `acr`.

A direct `acr freshness run` without `--policy` uses the `freshness` value stored in `agents.yaml`. An explicit `--policy` overrides the stored value for that invocation.

`outdated` is read-only: it reports newer stable releases only for dependencies declared as `latest`. `install` first reconciles those `latest` dependencies, then applies the normal transactional realization path. Explicit tag and commit pins are not advanced by either policy. If hook configuration or realized package content changes, a `restart_required` notice names the affected agents. JSON results keep every realized agent in `agents` and place only the affected subset in `restartAgents`.

Remote checks are limited to one attempt per project and policy in each 24-hour window. The machine-local record is stored outside the project at `${ACR_STATE_HOME:-<user-cache>/acr}/freshness/<project-key>.json`; the key uses the canonical project path, so different path spellings of one checkout share a timer. A policy change runs immediately. Missing, corrupt, or unsupported state is treated as no prior attempt and rewritten after the run. A future `lastCheckedAt` is not throttled; the next check runs and rewrites it with the current attempt time.

The direct `acr freshness run` command preserves the normal process exit contract: operational, network, authentication, update, state-write, and lock-release failures exit `1`; preservation or ownership conflicts use the [exit-4 contract](#exit-4-refusal-codes); lock contention exits `0`. `--policy none` runs no check and exits `0` even when `agents.yaml` cannot be read, and reports that unreadable project state as a `freshness_update_failed` notice rather than staying silent. The generated wrapper converts all of these outcomes to `0`.

## Release notice

On a terminal, before a command's own output, `acr` prints one line on stderr when a stable release newer than the running build exists:

```text non-executable
acr 0.2.0 is available (you have 0.1.0). Upgrade: brew upgrade jbaruch/agentic-context-registry/acr
```

The upgrade command matches where this copy of `acr` lives, with symlinks resolved. A path under a Homebrew `Cellar` names `brew upgrade jbaruch/agentic-context-registry/acr`. A binary directly inside the directory `go install` writes to — `GOBIN`, else the first `GOPATH` entry's `bin`, else `$HOME/go/bin` — names `go install github.com/jbaruch/agentic-context-registry/cmd/acr@latest`. Any other path names the releases page and the verification steps in [Installing acr](install.md). A path `acr` cannot resolve gets the two versions and no command. Detection inspects paths only; it runs no `brew` or `go` process.

The line is served from a machine-local cache, so no command waits on the network. At startup `acr` reads `${ACR_STATE_HOME:-<user-cache>/acr}/version/latest.json` and compares it with the running build. After the command's output, at most once per 24-hour window per machine, it fetches the latest stable GitHub release under a 2-second timeout and rewrites that record. The attempt is recorded in `attempt.json` beside it and serialized through the advisory lock `version.lock`, so concurrent processes perform one refresh between them. A timeout, an unreachable network, or an unusable response leaves the cache untouched, prints nothing, and is not retried inside the window; the command exits with its normal code either way. Two consequences follow: the notice can be up to a day behind, and a fresh install prints nothing on its first run because its cache is empty.

The notice is never printed, and no refresh runs, when the command carries `--json` or `--non-interactive`, when stdout or stderr is not a terminal, or when `ACR_VERSION_CHECK=off` is set. The check is on by default; `off` is the only value that disables it. `ACR_VERSION_CHECK` is machine-level and separate from the `freshness` policy in `agents.yaml`: that policy governs a project's dependencies and never affects this notice. Only stable releases count, so a pre-release never triggers it. A current build, a build newer than the latest release, a development build, and an unusable cache all print nothing.

`acr` never rewrites its own binary. It names the command; the operator runs it.

## Output contract

Human-readable results are written to stdout. Structured notices are written one per line to stderr. JSON mode echoes result notices in `result.notices`; if an application error replaces the result, the runner suppresses separate notice lines and writes one error object to stderr. Progress and diagnostics never contaminate the JSON document. Most commands write one success object to stdout or one error object to stderr. `acr freshness run --json` always writes its completed attempt as one result envelope on stdout. Its `ok` field matches the process exit code, while stderr notices describe the fail-open domain outcome.

Package resolution and migration errors in `acr install`, `acr update`, and `acr migrate tessl` escape non-printing characters so the message stays on one line. The message quotes release tags, versions, recorded commits, content hashes, and archive file names. Values longer than 128 bytes display with `...[truncated]`. Consumer validation shows the first issue and a count of any remaining issues. Run `acr validate` on the package to see every issue.

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
| `2` | Invalid command, flag, argument, or dependency source |
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

Validation resolves a relative `PATH` (default `.`) against `--project`; a relative project is resolved from the process working directory. After the project access check, an absolute `PATH` selects the package independently of `--project`. With neither argument, validation uses the working directory. For example, `acr validate nested --project ../producer` validates `../producer/nested`.

Producer migration (`acr migrate tessl-plugin`) resolves relative or omitted `PATH` against `--project` in both dual-distribution and `--acr-only` modes. A relative project resolves from the process working directory; after the project access check, an absolute package path selects the package independently of the project. With neither argument, migration uses the working directory. Clean migration still refuses positional `..` components; `--project ../producer` is a supported base selection.

Clean producer conversion maps the source identity through the existing closed validation before removing `source.tesslIdentity`. Supported source names use lowercase ASCII `owner/package`: the owner has 1–39 characters using letters, digits and internal hyphens; the package uses letters or digits with internal periods, underscores or hyphens. Both parts start and end with a letter or digit. A broader name accepted by Tessl lint can therefore refuse at that intermediate field even with a valid explicit destination.

The generated publisher reserves `.github/workflows/acr-publish.yml`. An existing file at that path refuses before writes, including a recognized Tessl publisher; in-place conversion at the reserved destination is unsupported.

### Deterministic producer conversion

ACR does not run agents or model CLIs. Clean conversion refuses before any write when a file needs changes beyond the deterministic rules. The refusal report lists every such file and its reason. Migrate the listed files manually with your own agent in your own sandbox, review the result, and rerun the same conversion without the removed flags.

Breaking upgrade: producer migration no longer accepts agent selection or widening consent. Existing automation must remove those options. Native hook configurations that omit an ACR adapter must be reconciled manually before conversion. Realization adapter selection on `init`, `install`, `realize`, and `check` is unchanged; those adapters write files for agents to read.

See [manual migration](migration-producer.md#manual-migration) for the handoff and receipt upgrade requirements.

`acr install SOURCE@VERSION --if-missing` supports setup helpers that must preserve an existing dependency's request, rollback hold and locked resolution. It resolves missing state through the existing policy and adds a declaration only if absent. It requires an explicit source and cannot combine with `--hold` or `--pin`. Existing pins and holds are never replaced by the supplied fallback version.

## Local plugin development

```console
$ acr install ./my-plugin --agent codex --freshness none --non-interactive
# fixture: local-source
# exit: 0
Dependency state updated in agents.yaml and .agents/registry.lock; run 'acr realize' to materialize locked artifacts.
```

Run `acr realize` to materialize the locked inventory. After editing a declared
plugin file, run bare `acr install` to refresh, then `acr realize` again.
`acr install file:../my-plugin` selects a sibling directory.

`acr install my-plugin`, with no `./` or `file:`, is an invalid source and
exits `2`. The diagnostic keeps the local-directory hint `./my-plugin`.
`acr install ./my-plugin` selects a local directory. Paths resolve against
`--project`, including `.` and `..`; an absolute path stays absolute. Local `@`
is a filename character. `file://` URLs are unsupported. Local paths cannot be
combined with `--hold`, `--pin`, or `--if-missing`.

Local state is explicitly marked and warned about. Each explicit path install
creates a schema-1 authorization record under
`${ACR_STATE_HOME:-<user-cache>/acr}/local/<project-key>/<source-key>.json`, with
0600 record permissions and 0700 directories. The store must be outside the
project and plugin source. `list` reports authorization status without adopting
it. `realize`, `check`, bare `install`, and session-start install refuse a copied
local row without this record. The directory is authorized, not its bytes:
re-install to refresh edits; locked realization refuses edits until refresh.

A nested package root installs locally, but GitHub installation still requires
that manifest at its repository root. See [local dependencies](dependencies.md#local-path-declarations)
for inventory, refresh, ownership, and switching to a release.

Local authorization must remain outside both the project and the plugin.
Implicit installs, `migrate tessl`, and `uninstall` check local authorization
before recovering an interrupted project journal; use an explicit
`acr install file:PATH` to authorize and repair. Without a pending journal,
an empty migration remains a no-op and uninstall can remove an unauthorized or
unavailable last local package.
Switching a local row to a GitHub release without a pending journal still works
when the local source or its authorization is missing.

Offline uninstall does not create an authorization store. If a concurrent local
install creates that store during removal, ACR reports that project work completed
but authorization conflicted. Inspect the project and `ACR_STATE_HOME` before
retrying; that error does not mean the concurrent grant was revoked.
