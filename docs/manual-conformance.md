# Manual conformance checks

Two things the deterministic suite cannot establish: that the shipped binary
agrees with the real GitHub API, and that a real agent loads what `acr realize`
writes. Both are run by hand, on purpose, and recorded. Neither runs in CI, and
neither is a substitute for [the deterministic gates](testing.md).

Each check is read-only against public data. Nothing here publishes a release,
pushes a branch, or writes to a repository the operator does not own.

The two checks use **separate scratch consumers** and do not share state. Check
1 needs the network and ends by uninstalling everything it installed; check 2
needs no network at all and must keep its package installed while an agent looks
at it. Running check 2 in check 1's consumer would leave nothing to observe.

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

This check runs in **its own scratch consumer**, on a purely local package.
Nothing is published, nothing is downloaded, and it does not depend on check 1
having run.

### The canary package

[`canary-package/`](canary-package/) is a Tessl plugin carrying exactly the four
artifacts the observations need, and nothing else:

| Artifact | Sentinel to look for |
| --- | --- |
| `rules/always.md`, always-on | `ACR-CANARY-ALWAYS-7f3a` |
| `rules/scoped.md`, scoped to `docs/**` | `ACR-CANARY-SCOPED-7f3a` |
| `skills/acr-canary/SKILL.md` | `ACR-CANARY-SKILL-7f3a` |
| `hooks/session-start.sh` | `ACR-CANARY-SESSION-START-7f3a` |

The hook appends one marker line per session start to
`.acr-canary/session-start.log` inside the scratch consumer, or to
`$ACR_CANARY_LOG` when that is set. It reads nothing, runs no tool, reaches no
network, and writes nowhere else — a failed append warns on stderr and exits `0`
rather than breaking the session it is observing. Every artifact asks the agent
only to quote its sentinel.

### Phases, in this order

Steps 1 to 5 leave the fixture on disk; the observations in step 6 need it
there; only step 7 removes it. Running the teardown early is the one mistake
this ordering exists to prevent.

1. **Isolate.** A fresh scratch consumer directory outside any working tree, its
   own `ACR_STATE_HOME` and `TMPDIR`, a throwaway agent profile or container,
   and agent trust scoped to that directory. Never the operator's own
   configuration.
2. **Seed a human file.** Write `AGENTS.md` with known bytes at a known mode
   (`0640` is what the preparation check uses), so step 7 has something whose
   preservation can be checked.
3. **Show the three adapters.** Create `.claude/skills`, `.codex/skills` and
   `.cursor/skills`. Migration selects the agents it detects, so a consumer that
   does not show all three gets a canary realized for fewer than three.
4. **Install the canary locally.** Copy `canary-package/` to
   `.tessl/plugins/acr/canary`, write a `tessl.json` declaring
   `acr/canary` at version `1.0.0`, then vendor it:

   ```text non-executable
   acr migrate tessl --vendor-unmapped --non-interactive
   acr realize
   acr check
   ```

   `acr check` must report current before an agent is started. A canary that is
   not fully realized turns every later observation into a setup question.
5. **Confirm what the agent will be asked about exists.** Per adapter directory
   `.claude`, `.codex`, `.cursor`:
   - `<agent>/skills/acr__acr__canary__acr-canary/SKILL.md` is nonempty.
   - `<agent>/hooks/acr__acr__canary__session-start/session-start.sh` is `0755`.
   - the hook is registered in `.claude/settings.json`, `.codex/config.toml` and
     `.cursor/hooks.json`.
   - `AGENTS.md` and `CLAUDE.md` carry the always-on sentinel, and the operator's
     own bytes from step 2 are still there.
   - `.cursor/rules/acr__acr__canary__scoped.mdc` carries `alwaysApply: false`
     and the `docs/**` glob.
6. **Observe, per agent.** Only now start each agent in the scratch consumer.
7. **Tear down.** `acr uninstall vendor:acr/canary`, confirm the human
   `AGENTS.md` bytes and mode from step 2 survived and the canary artifacts are
   gone, then delete the scratch consumer.

Steps 1 to 5 and step 7 are executed by
[`cmd/acr/canary_test.go`](../cmd/acr/canary_test.go) on every ordinary test
run, against this same fixture directory, so the preparation is a procedure that
runs rather than one that was written down. Step 6 is the manual half and stays
manual: no test starts an agent.

### Observations, and how each one is captured

For each agent — Claude Code, Codex, Cursor — record all five. The capture
method is what makes the answer checkable by someone who was not there.

| Observation | How it is captured | Fails when |
| --- | --- | --- |
| Always-on rule reached the session | Ask the agent to quote the canary always-on sentinel; paste its reply | The agent cannot produce `ACR-CANARY-ALWAYS-7f3a` |
| Skill is discoverable | Ask the agent to list its available skills, then to run `acr-canary`; paste both replies | The skill is absent from the list, or invoking it does not produce `ACR-CANARY-SKILL-7f3a` |
| Session-start hook dispatched | `cat .acr-canary/session-start.log` after the session starts; paste the file and the line count | The file is absent, or holds no `ACR-CANARY-SESSION-START-7f3a` line. **A registered hook that never ran is a fail, not a pass** |
| Cursor scoped rule attaches on its glob | Open a file under `docs/` in the scratch consumer, ask which canary rules are in scope, then repeat outside `docs/`; paste both replies | The scoped sentinel is missing under `docs/`, or present outside it |
| Trust or permission prompts | Screenshot or transcript, quoted verbatim | — recorded either way; a prompt is not itself a failure |

Record the agent's build or version string beside each result: the answer is
version-specific, and a result with no build recorded cannot be compared to the
next run. Pass requires a nonempty observation for every row. "No error
appeared" is not an observation. Classify pass / fail / blocked exactly as in
check 1; an agent the operator has no licence or runtime access for is
**blocked**, never a pass.
