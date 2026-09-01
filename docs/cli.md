# CLI reference

The executable and shell command are named `acr`. The command layer parses user input, renders output, and dispatches typed invocations to application services. Dependency resolution and realization logic remain outside command handlers.

## Commands

| Command | Contract | Domain implementation |
| --- | --- | --- |
| `acr init` | Initialize project agent and freshness selections | Agent detection and realization in #10 and #12; freshness hooks in #16 |
| `acr install [SOURCE[@VERSION]]` | Resolve one package, or reconcile declared dependencies when no source is supplied | Resolution available; realization in #7 |
| `acr realize` | Reapply locked packages without remote resolution | Transactional realization in #7 |
| `acr list` | List declared and resolved dependencies | Available |
| `acr outdated` | Check `latest` dependencies without modifying project state | Available |
| `acr update [SOURCE]` | Update one dependency or all eligible dependencies | Resolution available; realization in #7 |
| `acr uninstall SOURCE` | Remove a dependency and its owned artifacts | Ownership and realization in #7 |
| `acr check` | Report drift without applying changes | Planning in #7 |
| `acr publish [PATH]` | Validate and publish an immutable package | Publishing in #9 |
| `acr migrate tessl` | Migrate a Tessl consumer project | Migration in #1, #2, and #8 |

Every domain command supports `--help`, `--json`, and `--project PATH`. Mutating commands support `--dry-run`. `init`, `install`, and `migrate tessl` support `--non-interactive`.

## Installation policy

An unversioned source such as `github:owner/plugin` requests the `latest` stable release. An explicit suffix such as `@v1.2.3` or `@COMMIT_SHA` requests a fixed dependency. Running `acr install` without a source reconciles dependencies already declared in `agents.yaml`, including refreshing declarations whose requested policy is `latest`.

The resolver records the requested policy separately from the immutable release, commit, and content hash selected for the lockfile.

## Setup policy

Interactive `init` and first-install flows select detected agents and one session-start freshness policy:

1. `outdated` checks for updates and is the default.
2. `install` reconciles dependencies declared as `latest`.
3. `none` installs no freshness hook.

Use repeated `--agent NAME` flags and `--freshness outdated|install|none` to provide the same selections non-interactively. `--non-interactive` forbids prompts.

## Output contract

Human-readable results are written to stdout. Diagnostics are written to stderr. JSON mode writes one success object to stdout or one error object to stderr; progress and diagnostics never contaminate a successful JSON document.

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
| `4` | Managed and unmanaged project state conflicts |

## Platforms

CI compiles `acr` for macOS and Linux on amd64 and arm64. Native Windows is outside the MVP.
