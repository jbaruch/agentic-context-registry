# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr version [--json]` | Report the release version and source commit when known; `--version` and `-v` are aliases | Available |
| `acr init` | Initialize project agent and freshness selections | Agent detection and realization in #10 and #12; freshness hooks in #16 |
| `acr install [SOURCE[@VERSION]] [--hold \| --pin]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Resolution available; realization in #7 |
| `acr realize [--agent NAME] [--dry-run]` | Verify and reapply locked packages into selected native layouts | Available |
| `acr list` | List declared and resolved dependencies | Available |
| `acr outdated` | Check `latest` dependencies without modifying project state | Available |
| `acr freshness run [--policy POLICY]` | Run the throttled session-start policy for this project | Available |
| `acr update [SOURCE]` | Update one dependency or all eligible dependencies | Resolution available; realization in #7 |
| `acr resume SOURCE` | Clear a rollback hold and resume `latest` | Available |
| `acr uninstall SOURCE` | Remove a dependency and its owned artifacts | Transaction engine available; preservation adapters in #6 and #12 |
| `acr check [--agent NAME]` | Report native-layout drift without applying changes | Available |
| `acr publish [PATH] [--dry-run]` | Validate and publish an immutable package | Available |
| `acr migrate tessl --dry-run` | Inventory a Tessl consumer project without writing files | Dry-run inventory available; apply in #2; vendoring in #8 |
| `acr migrate tessl-plugin [PATH]` | Convert a Tessl plugin package to `agent-plugin.yaml` | Producer conversion in #11 |

Every domain command supports `--help`, `--json`, and `--project PATH`. Mutating commands support `--dry-run`. `install` accepts the mutually exclusive `--hold` and `--pin` rollback choices described under [rollback holds](#rollback-holds). `init`, `install`, and `migrate tessl` support `--non-interactive`. `realize` and `check` accept repeated `--agent claude-code|codex|cursor`; without flags, they use the sorted `agents` selection in `agents.yaml`. `acr migrate tessl-plugin` takes the plugin package root as a positional PATH, the same way `acr publish [PATH]` does, and accepts `--repository URL` and `--accept-agent-widening`. The standalone `version` command supports `--json` but has no project state.

Producer conversion is documented in the [producer migration reference](migration-producer.md).

## Realization

`acr realize` downloads each immutable locked commit, revalidates package identity, version, and content hash, renders the selected native layouts, and applies the resulting plan transactionally. It updates the ownership ledger under `realization` in `.agents/registry.lock` only after all file operations succeed. `--dry-run` returns the plan without writing files or the ledger.

`acr check` runs the same materialization, rendering, preservation, and planning path in read-only mode. It exits `0` when current, `3` when a conflict-free plan has unapplied changes, and `4` for managed/unmanaged or preservation conflicts. Adapter validation completes before the transactional engine is invoked.

An explicit `--agent` list overrides the persisted selection for that invocation and does not rewrite `agents.yaml`. A project with neither flags nor persisted agents fails with guidance to select an adapter. Interactive selection and freshness policy remain owned by the setup flow.

## Tessl migration

`acr migrate tessl --dry-run` reads `tessl.json`, installed plugin and tile manifests, `.tessl/RULES.md`, and native Tessl outputs, then prints a schemaVersion 1 inventory grouped by package. It performs no writes. Omitting `--dry-run` returns `not_implemented` and still writes nothing; apply is issue #2.

The report classifies each artifact as `migratable`, `ambiguous`, or `unsupported`, names preserved unmanaged spans, and lists whether each Tessl native agent tree is covered by an ACR adapter. `unmapped` is a project-level bucket for Tessl-owned files with no v1 home, not an artifact class. The inventory contract, ownership markers, and JSON shape are documented in [`docs/migration.md`](migration.md).

## Installation policy

An unversioned source such as `github:owner/plugin` requests the `latest` stable release. An explicit suffix such as `@v1.2.3` or `@COMMIT_SHA` requests a fixed dependency. Running `acr install` without a source reconciles dependencies already declared in `agents.yaml`, including refreshing declarations whose requested policy is `latest`.

The resolver records the requested policy separately from the immutable release, commit, and content hash selected for the lockfile. A successful non-dry-run install also persists the selected `--freshness` value; when no value has been stored or supplied, it persists `outdated`.

## Rollback holds

Installing an explicit reference that does not move a `latest` dependency forward is a rollback, and it needs a choice. Without one, `acr install SOURCE@REF` exits `2` with the code `downgrade_choice_required` and names both flags:

1. `--hold` keeps `requested: latest` and records a temporary rollback: the known-good pin plus the rejected release, which becomes the resume barrier.
2. `--pin` replaces `latest` with a permanent pin and removes any hold. This is the only sanctioned hold-to-pin conversion.

Passing neither is cancel's non-interactive form; there is no default. The flags are mutually exclusive and require an explicit `SOURCE@VERSION`.

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

Interactive `init` and first-install flows select detected agents and one session-start freshness policy:

1. `outdated` checks for updates and is the default.
2. `install` reconciles dependencies declared as `latest`.
3. `none` installs no freshness hook.

Use repeated `--agent NAME` flags and `--freshness outdated|install|none` to provide the same selections non-interactively. `--non-interactive` forbids prompts.

## Session-start freshness

For `outdated` and `install`, realization adds one ACR-owned `session-start` hook to every selected native adapter. `none` contributes no hook and removes only the previously owned ACR hook on the next realization. User hooks and Codex `hooks.state` trust data are preserved.

The generated wrapper runs:

```sh
acr freshness run --project PROJECT --policy outdated|install
```

The wrapper never prompts and always exits `0`, so a missing binary, network failure, update failure, or ownership conflict cannot block agent startup. A throttled or no-change run emits nothing. When there is a status, the wrapper injects one native session-start context payload beginning `Session-start status — `; Claude Code and Codex receive `hookSpecificOutput.additionalContext`, while Cursor receives `additional_context`. Set `ACR_BIN` when the executable is not discoverable as `acr`.

A direct `acr freshness run` without `--policy` uses the `freshness` value stored in `agents.yaml`. An explicit `--policy` overrides the stored value for that invocation.

`outdated` is read-only: it reports newer stable releases only for dependencies declared as `latest`. `install` first reconciles those `latest` dependencies, then applies the normal transactional realization path. Explicit tag and commit pins are not advanced by either policy. If hook configuration or realized package content changes, a `restart_required` notice names the affected agents. JSON results keep every realized agent in `agents` and place only the affected subset in `restartAgents`.

Remote checks are limited to one attempt per project and policy in each 24-hour window. The machine-local record is stored outside the project at `${ACR_STATE_HOME:-<user-cache>/acr}/freshness/<project-key>.json`; the key uses the canonical project path, so different path spellings of one checkout share a timer. A policy change runs immediately. Missing, corrupt, or unsupported state is treated as no prior attempt and rewritten after the run.

The direct `acr freshness run` command preserves the normal process exit contract: operational, network, authentication, update, state-write, and lock-release failures exit `1`; preservation or ownership conflicts exit `4`; lock contention exits `0`. The generated wrapper converts all of these outcomes to `0`.

## Output contract

Human-readable results are written to stdout. Structured notices are written one per line to stderr. JSON mode echoes the same notices in `result.notices`; progress and diagnostics never contaminate the JSON document. Most commands write one success object to stdout or one error object to stderr. `acr freshness run --json` always writes its completed attempt as one result envelope on stdout. Its `ok` field matches the process exit code, while stderr notices describe the fail-open domain outcome.

Success envelope:

```json
{"ok":true,"command":"list","result":{}}
```

Error envelope:

```json
{"ok":false,"command":"install","error":{"code":"operation_failed","message":"..."}}
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Command, help, or version completed successfully |
| `1` | Operational failure, including producer conversion refusals (`unknown_field`, `unmapped_field`, `agent_widening`, `ambiguous_manifest`, `manifest_conflict`, `unpublishable_content`, and #4 validation codes) |
| `2` | Invalid command, flag, or argument |
| `3` | `check` found unapplied changes |
| `4` | Managed and unmanaged project state conflicts, including freshness realization conflicts |

## Platforms

Tagged releases publish `acr-darwin-amd64.tar.gz`, `acr-darwin-arm64.tar.gz`, `acr-linux-amd64.tar.gz`, and `acr-linux-arm64.tar.gz`. Each candidate runs on its native CI runner before publication. Homebrew installation is tested on macOS and Linux. See [Installing acr](install.md) for Homebrew, verified direct downloads, `go install`, and the macOS Gatekeeper validation procedure. Native Windows is outside the MVP.
