# Agentic Context Registry

ACR is a package manager for AI coding agent instructions. A package is a GitHub repository. Installing one writes the files that Claude Code, Codex, and Cursor each read, and leaves your own files alone. The command is `acr`.

**Pre-alpha.** Everything below works today on macOS and Linux. Expect breaking changes before 1.0.

## Install

```shell
brew install jbaruch/agentic-context-registry/acr
```

Verified direct downloads and `go install` are covered in [Installing acr](docs/install.md).

## Try it

Two commands, in any project directory:

```text non-executable
acr install github:OWNER/REPO
acr realize
```

The first `acr install` asks which agents you use. `acr realize` then writes their files. [What a real run looks like](#what-a-real-run-looks-like) shows exactly what lands on disk.

Any GitHub repository with an `agent-plugin.yaml` and a Release is a package. The project's own test package, `github:jbaruch/ffa-acr-dogfood`, is one you can install right now.

## Supported agents

| Agent (`--agent` name) | Version | Boundary | Rules | Skills | Scripts | Hooks |
| --- | --- | --- | --- | --- | --- | --- |
| `claude-code` | `1.0.3` | `1` | Yes | Yes | Yes | Yes |
| `codex` | `1.0.3` | `1` | Yes | Yes | Yes | Yes |
| `cursor` | `1.0.2` | `1` | Yes | Yes | Yes | Yes |

The last four columns are the kinds of content a package can carry. Every agent takes all four. **Version** is the version of that agent's adapter, the ACR code that writes its files: the Claude Code adapter writes `.claude/` and the `CLAUDE.md` block. It changes when the shape of those files changes. **Boundary** is the version of the interface between adapters and ACR's core. ACR refuses to load an adapter at any other number. Neither is ACR's own release number. Details are in [How ACR writes each agent's files](docs/adapters.md).

### Deferred capabilities

- Native Windows is tracked in [issue #14](https://github.com/jbaruch/agentic-context-registry/issues/14). WSL counts as Linux and uses the Linux build.
- Hosted accounts: there are none. Everything lives in your project's `agents.yaml` and `.agents/registry.lock`. Scope is recorded in [issue #13](https://github.com/jbaruch/agentic-context-registry/issues/13).
- MCP server configuration: packages cannot carry it ([issue #4](https://github.com/jbaruch/agentic-context-registry/issues/4)), and ACR never writes an MCP entry. The Tessl migration removes only the Tessl MCP entry it can identify with certainty. Every other entry is left byte for byte ([issue #94](https://github.com/jbaruch/agentic-context-registry/issues/94)).

## CLI

```text non-executable
acr init
acr install github:OWNER/REPO
acr realize
acr check
acr list
acr outdated
acr update
acr freshness run
acr resume github:OWNER/REPO
acr uninstall github:OWNER/REPO
acr validate
acr publish
acr migrate tessl-plugin
acr migrate tessl
```

Run `acr help COMMAND` for the options of any command.

Set up and install:

- `acr init` picks the agents to write for and the update policy. The first `acr install` in a fresh project asks the same questions.
- `acr install github:OWNER/REPO` adds a package. Add `@v1.2.3` or `@COMMIT` to pin it. Without a version it follows the latest release.
- Private repositories work too. ACR reads `GH_TOKEN`, then `GITHUB_TOKEN`, then `gh auth token`, then your Git credentials.
- `acr realize` writes the agent files for every package in `agents.yaml`.

Keep it current:

- `acr outdated` reports newer releases and changes nothing.
- `acr update` moves `latest` packages forward. Pins stay put.
- `acr check` reports whether the files on disk match what `acr realize` would write. Exit `3` means they do not.
- `acr install github:OWNER/REPO@v1.2.3 --hold` rolls a broken `latest` package back to a known-good version and blocks the release that broke it. `acr resume github:OWNER/REPO` lifts the block.
- `acr freshness run` is the command the session-start hook calls to check for updates.

Remove:

- `acr uninstall github:OWNER/REPO` removes the package and everything it wrote. Your own text stays.

Publish your own package:

- `acr validate` checks an `agent-plugin.yaml` and the files it names, offline.
- `acr publish` creates a GitHub Release for a tagged commit so others can install it.

Move off Tessl:

- `acr migrate tessl-plugin` converts a Tessl plugin into an ACR package. `--acr-only --agent codex` or `--agent claude` requests semantic proposals from the configured account; the [CLI reference](docs/cli.md#semantic-producer-conversion) covers the validation boundary.
- `acr migrate tessl` moves a project from Tessl to ACR in stages. Tessl keeps working until `--finalize` removes it.

Every command that changes files takes `--dry-run`: it prints the plan and writes nothing. Every command that reads a project takes `--json` and `--project PATH`.

## What a real run looks like

Before: a directory holding one file, `CLAUDE.md`, with two lines of your own notes.

The run, with the setup questions answered by flags and the version pinned so the output stays reproducible:

```text non-executable
$ acr install github:jbaruch/ffa-acr-dogfood@v0.9.39 --agent claude-code --agent codex --agent cursor --freshness none --non-interactive
Dependency state updated in agents.yaml and .agents/registry.lock; run 'acr realize' to materialize locked artifacts.
$ acr realize
Applied 56 realization change(s) for claude-code, codex, cursor.
```

After, trimmed to one line per kind of file:

```text
agents.yaml              your request: which agents, which packages
.agents/registry.lock    what was installed: release tag, commit, content hash
CLAUDE.md                your two lines, plus a block of rules ACR manages   (Claude Code reads this)
AGENTS.md                the same rules in a block ACR manages               (Codex reads this)
.cursor/rules/*.mdc      the same rules, one file each                       (Cursor reads these)
.claude/skills/  .codex/skills/  .cursor/skills/              the package's skills, one copy per agent
.claude/hooks/   .codex/hooks/   .cursor/hooks/               the package's hooks
.claude/settings.json  .codex/config.toml  .cursor/hooks.json   hook wiring, merged into any settings you already have
```

Every file ACR generates has a name starting `acr__OWNER__REPO__`, so you can always tell what it wrote. Shared files such as `CLAUDE.md` get a marked block instead, and nothing outside the block is touched.

Undo is one command:

```text non-executable
$ acr uninstall github:jbaruch/ffa-acr-dogfood
Removed github:jbaruch/ffa-acr-dogfood@v0.9.39; deleted 53 target(s) and spliced 1 shared target(s) for claude-code, codex, cursor.
```

`CLAUDE.md` is back to your two lines, byte for byte.

## What ACR will and will not touch

- ACR writes files it generated, its own marked blocks and entries inside shared files, and its two state files. Nothing else. The per-command list is in [What each command may change, and how to undo it](docs/safety.md).
- Text you wrote around an ACR block in `CLAUDE.md` or `AGENTS.md` survives every install, update, and uninstall. Worked examples are in [Shared instruction files](docs/shared-files.md).
- Generated files are hidden from Git locally through `.git/info/exclude`. Shared files such as `CLAUDE.md` are yours to commit.
- Every change is journaled. If a run is interrupted, the next command that writes rolls the half-applied files back before doing anything else.
- `.agents/registry.lock` records each package's release tag, commit, and content hash. `acr realize` downloads that exact commit and re-checks the package name, version, and content hash before writing.

## Where the files go

- Claude Code: rules go into a block ACR manages in `CLAUDE.md`. Skills, scripts, and hooks go under `.claude/`, and hooks are registered in `.claude/settings.json`.
- Codex: rules go into a block ACR manages in `AGENTS.md`. Skills, scripts, and hooks go under `.codex/`, and hooks are registered in `.codex/config.toml`.
- Cursor: rules go into `.cursor/rules/<name>.mdc`, one file per rule. Skills, scripts, and hooks go under `.cursor/`, and hooks are registered in `.cursor/hooks.json`.

ACR has no adapter for Gemini, VS Code, GitHub, or OpenHands, so it never writes to `.gemini`, `.vscode`, `.github`, or `.openhands`. ACR never realizes or removes them, including during a Tessl migration.

Some tools read skills from a shared `.agents/skills/` folder rather than an agent-specific one. Set `sharedSkills: true` in `agents.yaml` and ACR also copies every package skill there, as `.agents/skills/acr__OWNER__REPO__<skill>`. Only skills are copied. Rules and hooks stay out, because a generic reader has no way to run hooks or scope rules. `acr migrate tessl` turns this on when the Tessl project already used that folder. `acr init` never does.

## Read more

Using acr:

- [CLI reference](docs/cli.md): every command, flag, exit code, and output format.
- [What each command may change, and how to undo it](docs/safety.md).
- [Troubleshooting](docs/troubleshooting.md): each error code and the command that fixes it.
- [Dependency declarations and lockfile](docs/dependencies.md): the `agents.yaml` and `.agents/registry.lock` formats, pins, and holds.
- [Installing acr](docs/install.md): Homebrew, verified downloads, `go install`, and macOS Gatekeeper.

What lands on disk:

- [How ACR writes each agent's files](docs/adapters.md): the adapter reference.
- [Shared instruction files](docs/shared-files.md): `CLAUDE.md` and `AGENTS.md` before and after, byte for byte.
- [Transactional realization and ownership](docs/realization.md): the plan, the ownership record, the journal, and Git exclusion.

Making a package:

- [Author a plugin from an existing repository](docs/plugin-authoring.md): start here.
- [Package manifest](docs/package-manifest.md): the `agent-plugin.yaml` format, with its [JSON Schema](schemas/agent-plugin.schema.json).
- A [minimal](examples/minimal/agent-plugin.yaml) and a [complete](examples/complete/agent-plugin.yaml) example manifest.
- [Publishing packages](docs/publishing.md): release assets and the reusable GitHub Actions workflow.

Leaving Tessl:

- [Migration guide](docs/migration-guide.md): the staged path, plugin producers first, then each consumer project.
- [Migration reference](docs/migration.md): what is inventoried, what is removed, and what blocks finalization.

How it is tested:

- [Testing](docs/testing.md): the gates and what each one proves.
- [Manual conformance](docs/manual-conformance.md): the checks run by hand against real GitHub and real agents.

## Development

Go 1.27 or newer. The four gates every change runs:

```shell
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build ./cmd/acr
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## License

[Apache License 2.0](LICENSE).
