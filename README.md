# Agentic Context Registry

Agentic Context Registry is a GitHub-native package manager and materializer for agent rules, skills, scripts, and hooks. Its command-line interface is `acr`.

The project is an early-stage replacement for hosted agent-context registries. GitHub repositories and releases are the distribution layer; `acr` resolves packages and realizes them into each coding agent's native project layout without overwriting custom instructions.

## Status

The project is in pre-alpha development. The `acr` command framework, GitHub dependency resolution, crash-recoverable realization engine, native Claude Code/Codex/Cursor adapters, preservation-aware rendering, interactive project setup, dependency removal, immutable package publishing, and Tessl coexistence migration are available. The implementation plan is tracked in [GitHub Issues](https://github.com/jbaruch/agentic-context-registry/issues).

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

Windows, global installations, hosted accounts, and deprecated documentation artifacts are outside the MVP.

## CLI

The command and output contract is documented in the [CLI reference](docs/cli.md). Domain operations become functional as their owning implementation issues land.

```text
acr init
acr install github:owner/plugin
acr realize
acr list
acr outdated
acr update
acr resume github:owner/plugin
acr uninstall github:owner/plugin
acr check
acr publish
acr migrate tessl
acr migrate tessl-plugin
```

`acr init` detects the agents a project already uses, asks which to realize for, and records the session-start freshness policy; the first `acr install SOURCE` of an unconfigured project asks the same questions. `acr uninstall SOURCE` drops the declaration and its lock row and re-renders, so the removed package's outputs go and everything else stays.

An unversioned dependency means `latest`; the local lock records the concrete release, commit, and content hash. Explicit release and commit pins remain fixed. A `latest` dependency broken by a new release can be rolled back temporarily with `acr install SOURCE@REF --hold`, which keeps `latest` behind a resume barrier that only `acr resume SOURCE` returns from.

The project declaration and immutable lock formats are documented in the [dependency reference](docs/dependencies.md).

The realization planner, ownership ledger, transactional apply modes, and local Git-exclusion behavior are documented in the [realization reference](docs/realization.md).

Deterministic GitHub Release assets and the reusable publishing workflow are documented in [Publishing packages](docs/publishing.md).

CLI installation through Homebrew, verified direct downloads, or Go is documented in [Installing acr](docs/install.md).

Tessl consumer migration, including explicit mapping, offline vendoring of unmapped packages, coexistence warnings, and recoverable finalization (`acr migrate tessl --vendor-unmapped` / `--finalize`), is documented in the [migration reference](docs/migration.md).

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
