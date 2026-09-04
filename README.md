# Agentic Context Registry

Agentic Context Registry is a GitHub-native package manager and materializer for agent rules, skills, scripts, and hooks. Its command-line interface is `acr`.

The project is an early-stage replacement for hosted agent-context registries. GitHub repositories and releases are the distribution layer; `acr` resolves packages and realizes them into each coding agent's native project layout without overwriting custom instructions.

## Status

The project is in pre-alpha development. The implemented MVP surface is documented below; remaining work is tracked in [GitHub Issues](https://github.com/jbaruch/agentic-context-registry/issues).

## MVP

The first release targets macOS and Linux and will provide:

- Public and private GitHub package sources
- First-class `latest`, exact release, and commit policies
- Rules, skills, scripts, and hooks as coherent plugin units
- Claude Code, Codex, and Cursor realization
- Preservation-safe handling of existing agent instructions and native configuration
- Transactional install, update, rollback, and uninstall operations
- Migration from Tessl consumer projects and plugin manifests
- [Signed GitHub Release binaries and Homebrew distribution](docs/install.md)

### Supported agents

Every shipped adapter implements the complete v1 artifact capability set:

| Adapter | Version | Boundary | Rules | Skills | Scripts | Hooks |
| --- | --- | --- | --- | --- | --- | --- |
| `claude-code` | `1.0.0` | `1` | Yes | Yes | Yes | Yes |
| `codex` | `1.0.0` | `1` | Yes | Yes | Yes | Yes |
| `cursor` | `1.0.0` | `1` | Yes | Yes | Yes | Yes |

Tessl-native `.gemini`, `.vscode`, `.github`, and `.agents/skills` trees are outside this adapter boundary. ACR never realizes or removes them.

### Deferred capabilities

- Native Windows is deferred to [issue #14](https://github.com/jbaruch/agentic-context-registry/issues/14). WSL counts as Linux and uses the Linux build.
- Global installations and hosted accounts remain outside the command-line MVP tracked in [issue #13](https://github.com/jbaruch/agentic-context-registry/issues/13).
- MCP configuration has no v1 artifact class in [issue #4](https://github.com/jbaruch/agentic-context-registry/issues/4). Existing MCP configuration is retained and never deleted.

## CLI

How the CLI is tested — the gates, the lanes, the per-command journey map, and the boundaries the deterministic suite does not cover — is documented in [Testing](docs/testing.md). The command and output contract is documented in the [CLI reference](docs/cli.md). The [safety contract](docs/safety.md) states what each command may create, overwrite, or remove and how to undo it. Common failures and exact recovery commands are indexed in [Troubleshooting](docs/troubleshooting.md).

```text non-executable
acr init
acr install github:owner/plugin
acr realize
acr list
acr outdated
acr update
acr freshness run
acr resume github:owner/plugin
acr uninstall github:owner/plugin
acr check
acr publish
acr migrate tessl
acr migrate tessl-plugin
```

Run `acr help COMMAND` for the exact invocation and options.

`acr init` detects the agents a project already uses, asks which to realize for, and records the session-start freshness policy; the first `acr install SOURCE` of an unconfigured project asks the same questions. `acr uninstall SOURCE` drops the declaration and its lock row and re-renders, so the removed package's outputs go and everything else stays.

An unversioned dependency means `latest`; the local lock records the concrete release, commit, and content hash. Explicit release and commit pins remain fixed. A `latest` dependency broken by a new release can be rolled back temporarily with `acr install SOURCE@REF --hold`, which keeps `latest` behind a resume barrier that only `acr resume SOURCE` returns from.

The project declaration and immutable lock formats are documented in the [dependency reference](docs/dependencies.md).

The realization planner, ownership ledger, transactional apply modes, and local Git-exclusion behavior are documented in the [realization reference](docs/realization.md). Worked examples in [Shared instruction files](docs/shared-files.md) show how custom `CLAUDE.md` and `AGENTS.md` content survives realization and removal.

Deterministic GitHub Release assets and the reusable publishing workflow are documented in [Publishing packages](docs/publishing.md).

Homebrew is the recommended way to install the CLI; verified direct downloads and Go developer installs are documented in [Installing acr](docs/install.md).

The [end-to-end migration guide](docs/migration-guide.md) covers producer preparation, consumer coexistence, vendoring, rollback, and finalization. The underlying inventory and normalization contract is documented in the [migration reference](docs/migration.md).

## Package format

Packages use a versioned, agent-neutral `agent-plugin.yaml` contract. See the [package manifest specification](docs/package-manifest.md), [JSON Schema](schemas/agent-plugin.schema.json), and checked-in [minimal](examples/minimal/agent-plugin.yaml) and [complete](examples/complete/agent-plugin.yaml) examples.

## Development

The repository requires Go 1.27 or newer.

```shell
go test -race ./...
go vet ./...
go build ./cmd/acr
```

Run `gofmt -w` on changed Go files before committing. See [CONTRIBUTING.md](CONTRIBUTING.md) before starting work.

## License

Licensed under the [Apache License 2.0](LICENSE).
