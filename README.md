# Agentic Context Registry

Agentic Context Registry is a GitHub-native package manager and materializer for agent rules, skills, scripts, and hooks. Its command-line interface is `acr`.

The project is an early-stage replacement for hosted agent-context registries. GitHub repositories and releases are the distribution layer; `acr` resolves packages and realizes them into each coding agent's native project layout without overwriting custom instructions.

## Status

The project is in pre-alpha design and bootstrap. The CLI is not ready for use yet. The implementation plan is tracked in [GitHub Issues](https://github.com/jbaruch/agentic-context-registry/issues).

## MVP

The first release targets macOS and Linux and will provide:

- Public and private GitHub package sources
- First-class `latest`, exact release, and commit policies
- Rules, skills, scripts, and hooks as coherent plugin units
- Claude Code, Codex, and Cursor realization
- Preservation-safe handling of existing agent instructions and native configuration
- Transactional install, update, rollback, and uninstall operations
- Migration from Tessl consumer projects and plugin manifests
- GitHub Release publishing and Homebrew distribution

Windows, global installations, hosted accounts, and deprecated documentation artifacts are outside the MVP.

## Planned commands

```text
acr init
acr install github:owner/plugin
acr list
acr outdated
acr update
acr uninstall github:owner/plugin
acr check
acr publish
acr migrate tessl
```

An unversioned dependency means `latest`; the local lock records the concrete release, commit, and content hash. Explicit release and commit pins remain fixed.

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
