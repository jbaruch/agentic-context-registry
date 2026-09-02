# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr init` | Initialize project agent and freshness selections | Agent detection and realization in #10 and #12; freshness hooks in #16 |
| `acr install [SOURCE[@VERSION]]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Resolution available; realization in #7 |
| `acr realize [--agent NAME] [--dry-run]` | Verify and reapply locked packages into selected native layouts | Available |
| `acr list` | List declared and resolved dependencies | Available |
| `acr outdated` | Check `latest` dependencies without modifying project state | Available |
| `acr freshness run [--policy POLICY]` | Run the throttled session-start policy for this project | Available |
| `acr update [SOURCE]` | Update one dependency or all eligible dependencies | Resolution available; realization in #7 |
| `acr uninstall SOURCE` | Remove a dependency and its owned artifacts | Transaction engine available; preservation adapters in #6 and #12 |
| `acr check [--agent NAME]` | Report native-layout drift without applying changes | Available |
| `acr publish [PATH]` | Validate and publish an immutable package | Publishing in #9 |
| `acr migrate tessl` | Migrate a Tessl consumer project | Migration in #1, #2, and #8 |

Every domain command supports `--help`, `--json`, and `--project PATH`. Mutating commands support `--dry-run`. `init`, `install`, and `migrate tessl` support `--non-interactive`. `realize` and `check` accept repeated `--agent claude-code|codex|cursor`; without flags, they use the sorted `agents` selection in `agents.yaml`.

## Realization

`acr realize` downloads each immutable locked commit, revalidates package identity, version, and content hash, renders the selected native layouts, and applies the resulting plan transactionally. It updates the ownership ledger under `realization` in `.agents/registry.lock` only after all file operations succeed. `--dry-run` returns the plan without writing files or the ledger.

`acr check` runs the same materialization, rendering, preservation, and planning path in read-only mode. It exits `0` when current, `3` when a conflict-free plan has unapplied changes, and `4` for managed/unmanaged or preservation conflicts. Adapter validation completes before the transactional engine is invoked.

An explicit `--agent` list overrides the persisted selection for that invocation and does not rewrite `agents.yaml`. A project with neither flags nor persisted agents fails with guidance to select an adapter. Interactive selection and freshness policy remain owned by the setup flow.

## Installation policy

An unversioned source such as `github:owner/plugin` requests the `latest` stable release. An explicit suffix such as `@v1.2.3` or `@COMMIT_SHA` requests a fixed dependency. Running `acr install` without a source reconciles dependencies already declared in `agents.yaml`, including refreshing declarations whose requested policy is `latest`.

The resolver records the requested policy separately from the immutable release, commit, and content hash selected for the lockfile. A successful non-dry-run install also persists the selected `--freshness` value; when no value has been stored or supplied, it persists `outdated`.

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

The wrapper never prompts, emits nothing on stdout, and always exits `0`, so a missing binary, network failure, update failure, or ownership conflict cannot block agent startup. It writes actionable notices to stderr. Set `ACR_BIN` when the executable is not discoverable as `acr`.

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
| `1` | Operational failure |
| `2` | Invalid command, flag, or argument |
| `3` | `check` found unapplied changes |
| `4` | Managed and unmanaged project state conflicts, including freshness realization conflicts |

## Platforms

CI compiles `acr` for macOS and Linux on amd64 and arm64. Native Windows is outside the MVP.
