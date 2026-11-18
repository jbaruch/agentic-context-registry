# Contributing

Thank you for helping build Agentic Context Registry. The project welcomes careful human and AI-assisted contributions.

## Before you start

1. Search the [open issues](https://github.com/jbaruch/agentic-context-registry/issues) for existing work and dependencies.
2. For a substantial change without an issue, open one before implementing it.
3. If an issue is already assigned or has an active pull request, coordinate before starting competing work.
4. If you do not own the repository, install the agent contribution policy:

   ```shell
   tessl install tessl-labs/good-oss-citizen
   ```

   `tessl install` appends a tessl-managed include to the tracked `AGENTS.md`: the heading `# Agent Rules <!-- tessl-managed -->`, then a line that starts with `@` and names `.tessl/RULES.md`. That include is the session loader for the good-oss-citizen rules, and Tessl may also add a `[mcp_servers.tessl]` stanza to `.codex/config.toml`; keep both while you work. A clone without `.tessl` fails realize with `unresolved_include` when the include is committed, and the stanza keeps realize printing `shared_file_requires_commit`. Before you submit, remove only those contributor-local additions: the heading and `@` include line from `AGENTS.md`, and the `[mcp_servers.tessl]` stanza from `.codex/config.toml`. Keep all project source and every hook entry, then run `go run ./cmd/acr realize` again. `.agents/registry.lock` is committed; realize maintains it, so never restore or prune it as cleanup.

5. Right after cloning, and before starting an agent session, realize the generated agent files. Owners and non-owners both run this, after step 4 when that step applies:

   ```shell
   go run ./cmd/acr realize
   ```

6. Read `AGENTS.md` and the realized rules before editing.

Installing the plugin is required for AI agents contributing to this repository who do not own it. Human contributors who do not use an agent do not need Tessl. Owners skip step 4.

## Development workflow

1. Fork the repository and create a focused branch named `<type>/<description>`, such as `feat/add-manifest-parser`.
2. Keep one logical change per commit. Use an imperative commit subject no longer than 72 characters.
3. Add outcome-based, deterministic tests for every shipped Go module.
4. Install Git and ensure `git` is on `PATH`; realization integration tests require it and never skip.
5. Install `jq` and ensure it is on `PATH`; the release workflow tests execute the workflow's own jq programs and never skip.
6. Install the pinned SBOM generator and ensure the directory `go install` writes to (`GOBIN`, otherwise `$(go env GOPATH)/bin`) is on `PATH`; the SBOM generation test consumes it from `PATH` instead of downloading it, refuses any other build of it, and never skips:

   ```shell
   go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0
   ```

7. Run the local gates (CI also requires diagnostics to pass before its test jobs):

   ```shell
   test -z "$(gofmt -l .)"
   go vet ./...
   go test -race ./...
   go build ./cmd/acr
   ```

8. Update user-facing documentation when behavior changes.
9. Open a focused pull request and complete its contribution declaration.

Do not skip failing checks, disable tests, or mix unrelated formatting and functional changes.

## Pull requests

Pull request titles use this format:

```text
<type>(<scope>): <imperative summary>
```

Examples:

```text
feat(resolver): add immutable release pins
fix(realizer): preserve custom agent instructions
docs(contributing): clarify local checks
```

The pull request body must explain the motivation, summarize the change, describe verification, and disclose whether AI assistance was used. The human contributor owns the final review and submission.

## Security

Do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting for this repository when available.
