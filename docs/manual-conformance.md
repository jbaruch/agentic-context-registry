# Manual conformance checks

Two things the deterministic suite cannot establish: that the shipped binary
agrees with the real GitHub API, and that a real agent loads what `acr realize`
writes. Both are run by hand, on purpose, and recorded. Neither runs in CI, and
neither is a substitute for [the deterministic gates](testing.md).

Each check is read-only against public data. Nothing here publishes a release,
pushes a branch, or writes to a repository the operator does not own.

## Cadence

Run both on the earlier of: **monthly**, or **any change to a modelled contract**
— the GitHub REST surface `internal/dependency` calls, the source archive
layout, the redirect origins, or an adapter's native output shape. A release
that changes install, realization or publication runs them before the tag.

Record every run: date, the identities below, and the observed outcome per step.
A step with no recorded observation did not happen.

## Identities to record

Before either check, record what was actually exercised:

```text non-executable
go version            # toolchain, GOOS/GOARCH
go build ./cmd/acr    # the binary under test
go version -m ./acr   # revision, and vcs.modified must be false
shasum -a 256 ./acr   # binary digest
./acr version --json  # the identity the binary reports
```

`vcs.modified=true` invalidates the run: the binary is not a commit anyone can
refer to. Rebuild from a clean tree.

## Check 1 — live binary lifecycle

Proves the shipped binary talks to real GitHub. It is the deterministic
publisher-to-consumer journey with the fixture removed from underneath it.

Isolation, required: a scratch consumer directory outside any working tree, its
own `ACR_STATE_HOME` and `TMPDIR`, and `GH_TOKEN` unset or holding a read-only
token. The scratch directory is removed afterwards whether or not the run
passed.

Run, against an immutable public release the operator designates — the recorded
run used `github:jbaruch/ffa-acr-dogfood@v0.9.38`:

```text non-executable
acr init --agent claude-code --agent codex --freshness outdated --non-interactive
acr install github:OWNER/REPO@TAG --non-interactive
acr list
acr realize
acr check
acr outdated
acr install --non-interactive   # repeat: reconcile changes nothing
acr realize                     # repeat: realization changes nothing
acr uninstall github:OWNER/REPO
acr uninstall github:OWNER/REPO  # repeat: refuses, changes nothing
```

Pass requires all of these, each observed and written down, none inferred:

- Every command above exits `0`, except the second `uninstall`, which exits `2`
  with the documented not-declared refusal and changes no file.
- The lock records a **nonempty** commit, release ID, tag and
  `sha256:` content hash, and they match the release GitHub serves.
- `acr realize` reports a **nonzero** change count and writes nonempty rules,
  skill companions and hooks; executable artifacts land at `0755` and the rest
  at `0644`.
- `acr check` reports current, and repeated `install` and `realize` leave a
  byte-and-mode-identical tree.
- An immutable pin reports no latest dependency to check.
- `acr uninstall` removes every ACR-owned target, splices shared files, restores
  a human-authored file's exact bytes and mode, and leaves operator files alone.
- No credential appears in any stream.

Classify the result as **pass**, **fail** (a named command, its exit code and
its output), or **blocked** (the check could not run — no network, no
designated repository). Blocked is not a pass.

An explicitly opt-in publisher smoke against a sandbox repository the operator
owns is the one part still missing here; it writes, so it stays out until it is
separately authorized.

## Check 2 — native agent canary

Proves an agent loads what realization wrote. The suite asserts the bytes, the
modes and the native configuration entries; no assertion reaches the agent.

Isolation, required: the scratch consumer from check 1, a throwaway agent
profile or container, and agent trust settings scoped to that directory. Never
the operator's own configuration.

For each agent the project supports — Claude Code, Codex, Cursor — start the
agent in the scratch consumer and record:

- Whether a realized always-apply rule is present in the session's context.
- Whether a realized skill is discoverable by the name in its `SKILL.md`.
- Whether the session-start hook the adapter registered actually dispatches, and
  what it printed. A registered hook that never runs is a fail, not a pass.
- For Cursor, whether the `.mdc` activation glob attaches the rule to a file it
  names.
- Any trust or permission prompt the agent raised, verbatim.

Pass requires a nonempty observation for every line above. "No error appeared"
is not an observation. Classify pass / fail / blocked exactly as in check 1, and
name the agent build for each result — the answer is version-specific.
