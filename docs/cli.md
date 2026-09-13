# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr version [--json]` | Report the release version and source commit when known; `--version` and `-v` are aliases | Available |
| `acr help [COMMAND]` | Show root help or the exact usage and options for one command | Available |
| `acr init [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]` | Initialize project agent and freshness selections | Available |
| `acr install [SOURCE[@VERSION]] [--hold \| --pin \| --if-missing] [--agent NAME] [--freshness POLICY] [--non-interactive] [--dry-run]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Available |
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
| `acr migrate tessl-plugin [PATH] [--dry-run] [--repository URL] [--acr-only [--package-version SEMVER] [--agent codex\|claude]] [--accept-agent-widening]` | Convert a Tessl plugin package to `agent-plugin.yaml` | Available |

Every domain command supports `--help`, `--json`, and `--project PATH`. Mutating commands support `--dry-run`. `install` accepts the mutually exclusive `--hold` and `--pin` rollback choices described under [rollback holds](#rollback-holds). `init`, `install`, and `migrate tessl` support `--non-interactive`, which selects the default answer and never stands in for an approval. `migrate tessl --finalize` accepts `--accept-reviewed-changes TOKEN`. `init`, `install`, `realize`, and `check` accept repeated `--agent claude-code|codex|cursor`; without flags, `realize` and `check` use the sorted `agents` selection in `agents.yaml`. `uninstall` accepts no `--agent`. `acr migrate tessl-plugin` takes the plugin package root as a positional PATH, the same way `acr publish [PATH]` does, and accepts `--repository URL` and `--accept-agent-widening`. Its `--acr-only` mode requires an explicit target repository and accepts `--package-version SEMVER`; both new flags are producer-only, and the version override requires clean mode. The standalone `version` command supports `--json` but has no project state.

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
```

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

`acr validate [PATH]` checks the authored manifest, artifact paths, skill support files and distribution inventory without Git, credentials, network calls or writes. `PATH` defaults to the current directory. JSON reports `valid`, `name`, `version` and the sorted `files`; invalid input exits `1`. Validation does not supply a review score or perform publication.

`acr publish [PATH]` validates `agent-plugin.yaml`, requires a clean Git worktree with exactly one version-matching tag at `HEAD`, builds release assets from that tag's committed blobs, realizes the resulting archive through every supported adapter, and verifies the remote tag before creating a GitHub Release. Omitted `PATH` or `.` selects `--project` (the current directory by default); a relative `PATH` resolves under that project. A relative `--project` resolves from the current directory. An absolute `PATH` takes precedence, even if the unused project does not exist. Dry-run and publication use the same selection.

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

Validation resolves a relative `PATH` (default `.`) against `--project`; a relative project is resolved from the process working directory. An absolute `PATH` takes precedence over `--project`. With neither argument, validation uses the working directory. For example, `acr validate nested --project ../producer` validates `../producer/nested`.

Producer migration (`acr migrate tessl-plugin`) resolves relative or omitted `PATH` against `--project` in both dual-distribution and `--acr-only` modes. A relative project resolves from the process working directory; an absolute package path takes precedence even when the unused project does not exist. With neither argument, migration uses the working directory. Clean migration still refuses positional `..` components; `--project ../producer` is a supported base selection.

Clean producer conversion maps the source identity through the existing closed validation before removing `source.tesslIdentity`. Supported source names use lowercase ASCII `owner/package`: the owner has 1–39 characters using letters, digits and internal hyphens; the package uses letters or digits with internal periods, underscores or hyphens. Both parts start and end with a letter or digit. A broader name accepted by Tessl lint can therefore refuse at that intermediate field even with a valid explicit destination.

The generated publisher reserves `.github/workflows/acr-publish.yml`. An existing file at that path refuses before writes, including a recognized Tessl publisher; in-place conversion at the reserved destination is unsupported.

### Semantic producer conversion

`acr migrate tessl-plugin PATH --acr-only --repository URL --agent codex` explicitly requests semantic proposals from the installed Codex CLI using its configured account; `--agent claude` selects Claude instead. Ordinary mode invokes no provider, and a failed provider never falls back to another account or provider. Each runs in a private temporary directory and receives bounded selected authored files, workflows, tests and ancestor notices as text.

Before reading credential contents or calling either provider, semantic input inventory refuses these basenames case-insensitively in the selected package, `.github`, and `tests`: `.env`, `.env.*` except exactly `.env.example` and `.env.sample`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`, `.npmrc`, `.netrc`, and `.pypirc`. The path-only diagnostic asks you to move credentials outside those input scopes before retrying. Git ignore rules do not override this refusal. The two example filenames remain usable and must contain placeholders. This conservative filename check does not detect secrets embedded in ordinary source or renamed credential files, and does not universally sanitize provider responses. Source and output confidentiality obligations still apply.

Codex currently requires macOS, `/usr/bin/sandbox-exec`, and exactly `codex-cli 0.153.2`. ACR disables user configuration, project rules, skill rendering, plugins, hooks, apps, web search, shell execution, delegation and the execution host behind Code Mode wrappers. A separate OS read boundary blocks inherited home instructions and skill files; ACR verifies actual read denial before sending source. Symlinked home instruction files and unsupported versions or platforms are refused. Claude runs with tools and MCP disabled. Neither provider receives permission to edit the source. Codex must return exactly one nonempty completed final message; a second completed message refuses even when empty or identical. Claude array and stream envelopes reject duplicate JSON keys at every nesting level before interpreting fields, while retaining ordinary native metadata and tool-free result-before-init compatibility.

ACR validates structured edits, original hashes and modes, syntax, independent test/job retention, manifest and publication inventory before its existing transaction applies anything. Python preservation includes discovered `testLogin` and `test_login` definitions, recognized assert/fail calls, explicit `raise` statements and registration. Checks belong to their original lexical owners, including populated nested definitions; another owner cannot compensate for a removed check. Each raise counts once regardless of exception or cause spelling. Messages and metadata references may adapt; implicit exceptions and unchanged-count substitutions remain outside this finite check. Editable shell and Go tests permit source-reference adaptation and leading ordinary comment changes; other executable-body changes refuse because ACR cannot establish their equivalence. Shell syntax checks use recognized `sh` or Bash declarations without running proposed code; unknown declarations with arguments refuse. A finite `sh` guard also refuses nonidentifier function definitions that macOS syntax-only checking accepts. These checks do not prove arbitrary program behavior. Large trees are split into runtime, instruction and delivery requests and combined before validation. Up to three validation rounds (at most nine native requests) may occur within one command; each request has a twenty-minute deadline and uses medium effort. A repair retains earlier scopes only when the failure identifies a later scope, regenerates every dependent scope, and validates the entire combined proposal against the original files again. Provider failures, unavailable credentials, malformed output, tool attempts and races fail before writes. A fresh regular file at mode `000`, including a retained file copied for semantic staging, returns path-specific `unsupported_file_mode` before receipt/stage creation and terminates without provider repair or retries. Deterministic unchanged readable `000` files and intended metadata deletion remain supported. Repository tests may retain foreign consumer-state filenames without granting shipped helpers access to those files.

Retained workflows preserve every top-level field except display `name` and the separately checked `jobs`; retained step jobs preserve every field except the separately checked `steps`, including original conditions, permissions, dependencies, timing, runner and unknown policy fields. A pure publisher may replace its execution fields with the pinned ACR reusable call and exact root inputs while preserving its other policy. Fields that cannot be represented by that call, such as job-level environment, defaults or timing, refuse; existing Tessl-only environment may be removed under the same directional rule. Environment entries may only lose existing `TESSL_*`/`SECRET_TESSL_*` credentials and existing `TESSL_*` items in `GH_AW_SECRET_NAMES`; unrelated values and order remain. Retained steps preserve their other fields, invocation counts and relative order; harmless inserted steps may shift their positions. Retirement or replacement refuses when surviving `needs` or expressions refer to the affected job or step, including literal bracket references; dynamic or whole-context access cannot establish independence. Expression boundaries honor single-quoted literals and doubled quotes, including multiple and multiline expressions; incomplete extraction refuses affected retirement. Retained output from standalone publisher translation is checked again for Tessl operations and uses the normal explicit-agent semantic route when needed. Only `tesslio/setup-tessl`, `tesslio/patch-version-publish` and `jbaruch/coding-policy/.github/actions/skill-review`, each with a nonempty tag or SHA ref, identify removable service actions. Checkout is supporting content only inside a proven service-only job. Alongside supported service actions, only the closed three-command run form `mkdir -p /tmp/gh-aw/NAME`, `cd /tmp/gh-aw/NAME`, `tessl install OWNER/NAME --yes` may be replaced or removed, with literal matching paths and ordinary whitespace. Additional commands, shell operators, expansions, redirections, traversal and extra step controls refuse.

Every original publisher-step `if` is checked before service filtering, retirement or later translation, including publishers in mixed and ordinary jobs. A corresponding retained publisher execution preserves its condition's YAML presence, type and value; supported correspondence uses the recognized publisher action or the exact standalone `run: acr publish .`. Distinct occurrences remain in source order. A matching condition on checkout or a validation-only step cannot substitute, and one publisher cannot discharge multiple guarded originals. Step-guarded publisher-job/workflow retirement and reusable replacement refuse before writes; ACR does not transfer a step guard to another scope. A retained guarded `acr publish .` remains supported. Leaving the old guarded Tessl action unchanged does not make its existing unsupported deterministic translation available.

For the supported pinned reusable rewrite, explicit effective caller permissions are a separate compatibility check from original-policy equality. Explicit job `permissions` overrides workflow `permissions`; an explicit job map never inherits missing entries from the workflow map. `write-all` or a literal map entry `contents: write` is sufficient. `read-all`, `{}`, an explicit map missing `contents`, `contents: read` or `contents: none`, and unsupported representations refuse. ACR never adds, broadens or rewrites permissions to satisfy this check. When both scopes omit permissions, structural acceptance leaves repository defaults and hosted capability unverified. Guard reachability and trigger guesses do not relax explicit insufficiency. A main-triggered reusable job with an unchanged false job guard and sufficient permissions remains a preservation positive; trigger/tag compatibility and hosted publication still require downstream verification.

Whole publisher-job retirement requires the original infrastructure-only shape: optional display name, ordinary `ubuntu-latest` or absent runner, service-only steps, wholly removable Tessl environment and optional unreferenced outputs. Conditions, permissions, dependencies, timing, custom runners and unknown policy prevent retirement, including deletion of the whole workflow. A disclosed paid-score-only job may retire with its scoring-only policy; a mixed publisher/score job retains publisher protection.

For gh-aw v3 locks from compiler v0.71.5 without imports or inlined bodies, ACR verifies the original frontmatter hash, requires distinct custom-step occurrences in source order within one unambiguous compiled job, retaining the original job placement, and matching descriptions in the proposed source and compiled workflow, preserves other configuration, then recomputes the hash. Only an originally verified source/lock pair permits retained step `WORKFLOW_DESCRIPTION` values to change; their presence and agreement with the final source remain required. Job-level description changes remain protected. Generated extra steps are permitted; original independent steps still retain their occurrences and order. Other compiler contracts or configuration changes fail with an explicit diagnostic.

Historical names, comments and public repository URLs alone neither trigger semantic conversion nor permit edits. Delivery edits support workflow YAML, Markdown sources paired with originally verified supported gh-aw locks, and `.github/aw/actions-lock.json` cleanup. The action lock must contain an object with an `entries` object and no duplicate keys; only original entries whose key, `repo` and `version` identify an exact retired service action may disappear. Retained entries, unknown fields and top-level metadata remain structurally unchanged, with no additions. Other `.github` formats, including CODEOWNERS and PR templates, are read-only: replacement, patch and removal refuse, and recognized operations in unsupported formats produce a named refusal before writes. Unchanged historical-only policy files remain supported. Mixed non-workflow edits retain complete original GitHub, GitLab, Bitbucket and raw GitHub repository URL tokens and their counts; token preservation does not prove arbitrary attribution prose equivalent. Review changed attribution meaning as part of output acceptance.

Semantic validation populates private staging directories before restoring their exact source modes, supporting unchanged read-only directories. Cleanup restores access only inside that private stage; original and applied modes remain unchanged.

`--dry-run` exposes the actual proposed file contents and policy changes without changing the source. A fresh invocation may receive a different proposal; no saved-plan apply is offered. JSON output includes the request digest in `agentRuns[].requestDigest` and bounded provider responses and diagnostics; raw requests are not included. Every combined-validation attempt is recorded in `notes` with its attempt number and the one-based `agentRuns` entries whose proposals were used, including cached earlier scopes. Failed attempts remain after a successful repair. A failure identifying one scope is also attached to its most recent used run, labeled ACR combined validation; it is not a native provider execution failure or a per-scope test result. Unlocated, ambiguous and multi-scope failures remain complete at report level without blaming one run. Native execution evidence is retained unchanged. Save this output as migration evidence. Verified recovered Codex websocket idle reconnects also appear in `agentRuns[].warnings`; unknown stream errors and incomplete responses are refused. Treat this output as source material with the same confidentiality as the input. The portable receipt records policy changes and the final file inventory, without provider transcripts.

ACR-only permits removal of Tessl-only paid scoring. Mapped protected source-manifest descriptions are exempt from policy declarations; agents still cannot edit those manifests. Each other changed paid-scoring file must carry concrete metadata and, when retained as text, a visible retirement/no-equivalent-score notice in the final combined output. The [finite declaration syntax](migration-producer.md#paid-scoring-declarations) also covers complete service-only deletion and opaque action locks. ACR validation has no score85 equivalent. Independent functional tests and code review remain required. Existing skill trees carry portable `.acr-package.json` metadata and byte-identical copies of ancestor license/notice files; their originals remain intact. Custom helper tests still need to verify semantic behavior: structural checks alone cannot prove that arbitrary generated code is equivalent. For example, an added step can write `PYTHONOPTIMIZE=1` to `$GITHUB_ENV` and affect later unchanged tests. This limitation remains tracked under [#117](https://github.com/jbaruch/agentic-context-registry/issues/117); generated output that disables independent tests still fails acceptance.

`acr install SOURCE@VERSION --if-missing` supports setup helpers that must preserve an existing dependency's request, rollback hold and locked resolution. It resolves missing state through the existing policy and adds a declaration only if absent. It requires an explicit source and cannot combine with `--hold` or `--pin`. Existing pins and holds are never replaced by the supplied fallback version.

### Codex runtime renewal

CLI maintainers review the supported Codex release monthly and before every CLI release. Record the review date, reviewer, current exact pin, supported platform and a decision to retain the pin or propose a separate version update in the release review or a focused maintenance issue. Retaining the pin must state the reason; do not silently relax the exact-version check.

Before a separately reviewed version update, repeat the existing Codex isolation and native-contract verification against the proposed exact runtime. Record its version, platform, complete argv and isolation evidence, plus positive and negative results for:

- Disabled configuration, tools, shell execution, delegation, plugins, hooks, apps and web search, including the execution host behind Code Mode wrappers.
- Denial of inherited home instructions and skill reads at the OS boundary, using actual read-denial canaries; unsupported or symlinked home inputs must still fail closed before source is sent.
- Strict event handling that refuses tool calls, unexpected errors and incomplete responses, while preserving only explicitly recognized recovered reconnect warnings.
- Bounded final output with exactly one accepted final event, a matching final output file and valid proposal JSON; missing, mismatched, extra or oversized output must refuse.

Use the existing `internal/producerconvert/codex_test.go` controls and the native isolation evidence approach. Save the commands, raw bounded results and canary outcomes with the retain/update decision. A new provider, platform or runtime contract needs separate scope and review; this procedure does not authorize an upgrade.
