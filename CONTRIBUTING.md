# Contributing

Thank you for helping build Agentic Context Registry. The project welcomes careful human and AI-assisted contributions.

## Before you start

1. Search the [open issues](https://github.com/jbaruch/agentic-context-registry/issues) for existing work and dependencies.
2. For a substantial change without an issue, open one before implementing it.
3. If an issue is already assigned or has an active pull request, coordinate before starting competing work.
4. Install the repository's agent contribution policy:

   ```shell
   tessl install tessl-labs/good-oss-citizen
   ```

5. Read `AGENTS.md` and the realized rules before editing.

Installing the plugin is required for AI agents contributing to this repository. Human contributors who do not use an agent do not need Tessl.

## Development workflow

1. Fork the repository and create a focused branch named `<type>/<description>`, such as `feat/add-manifest-parser`.
2. Keep one logical change per commit. Use an imperative commit subject no longer than 72 characters.
3. Add outcome-based, deterministic tests for every shipped Go module.
4. Run the local gates:

   ```shell
   test -z "$(gofmt -l .)"
   go vet ./...
   go test -race ./...
   go build ./cmd/acr
   ```

5. Update user-facing documentation when behavior changes.
6. Open a focused pull request and complete its contribution declaration.

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
