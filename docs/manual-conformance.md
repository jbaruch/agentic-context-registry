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

The hook appends one marker line per session start to `$ACR_CANARY_LOG`, or to
`.acr-canary/session-start.log` when that variable is unset. It reads nothing,
runs no tool, reaches no network, and writes nowhere else — a failed append
warns on stderr and exits `0` rather than breaking the session it is observing.
Every artifact asks the agent only to quote its sentinel.

**Always set `ACR_CANARY_LOG` to an absolute per-agent path.** The fallback path
is relative, so it resolves against whatever working directory the agent
happened to give the hook, not against the consumer. Verified: run from another
directory, the marker lands in that directory and the consumer's log stays
empty; with an absolute `ACR_CANARY_LOG` the same run lands in the evidence
directory regardless of where it started.

### Phases, in this order

Steps 1 to 5 leave the fixture on disk; the observations in step 6 need it
there; only step 7 removes it. Running the teardown early is the one mistake
this ordering exists to prevent.

1. **Isolate.** A fresh scratch consumer directory outside any working tree, its
   own `ACR_STATE_HOME` and `TMPDIR`, a throwaway agent profile or container,
   and agent trust scoped to that directory. Never the operator's own
   configuration.
2. **Seed a human file and the two scope probes.** Write `AGENTS.md` with known
   bytes at a known mode (`0640` is what the preparation check uses), so step 7
   has something whose preservation can be checked. Write
   `docs/canary-in-scope.md` and `canary-out-of-scope.md` — two ordinary files
   whose only job is to sit inside and outside the scoped rule's `docs/**` glob.
   Also create an evidence directory **outside the consumer**; every log and
   transcript below is copied there, because step 7 deletes the consumer.
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
6. **Observe, per agent.** Only now start each agent in the scratch consumer,
   one agent at a time, following **Session hygiene** and **Observations** below.
7. **Tear down.** Copy every log and transcript into the evidence directory
   first — the consumer is about to stop existing. Then
   `acr uninstall vendor:acr/canary`, confirm the human `AGENTS.md` bytes and
   mode from step 2 survived and the canary artifacts are gone, and delete the
   scratch consumer.

Steps 1 to 5 and step 7 are executed by
[`cmd/acr/canary_test.go`](../cmd/acr/canary_test.go) on every ordinary test
run, against this same fixture directory, so the preparation is a procedure that
runs rather than one that was written down. Step 6 is the manual half and stays
manual: no test starts an agent.

### Session hygiene, per agent

An observation is about **this** session in **this** runtime. Two things make
that attribution collapse: a marker another agent left behind, and context
carried over from an earlier session. Both are prevented before the agent
starts, not argued about afterwards.

**Close every other agent session first.** One agent at a time. A second
runtime open in the same consumer makes every marker ambiguous.

**Give the agent its own empty log, and prove it was empty.** Export an absolute
per-agent path in the shell that launches the agent, so the hook the agent
spawns inherits it:

```text non-executable
AGENT=claude-code
export ACR_CANARY_LOG="$EVIDENCE/$AGENT.session-start.log"
rm -f -- "$ACR_CANARY_LOG"
test ! -e "$ACR_CANARY_LOG" && echo "before: absent"
```

Record that `before:` line. Then start the agent, and only afterwards:

```text non-executable
wc -l < "$ACR_CANARY_LOG"
cat -- "$ACR_CANARY_LOG"
```

The row passes only when a log that did not exist before the session now holds
at least one `ACR-CANARY-SESSION-START-7f3a` line. Save the file to the evidence
directory under the agent's name.

**If the runtime cannot pass an environment variable to its hooks**, use the
shared log and prove an increment instead. Two preconditions the first run does
not get for free. A freshly prepared consumer has neither the log nor its
`.acr-canary/` parent — the hook creates them on its own first append, and until
an agent has run there is nothing to copy and nowhere to truncate — so the
fallback creates the parent, archives a prior log **only if one exists**, and
initializes the log itself. And the hook's fallback path is relative, so it
resolves against the working directory the agent hands it: start the agent with
the consumer as its working directory, or the marker lands somewhere that is not
the file below. `$EVIDENCE` is the step 2 directory, outside the consumer.

```text non-executable
CONSUMER=...   # the scratch consumer from step 1; the agent starts here
EVIDENCE=...   # the evidence directory from step 2, outside the consumer
SHARED="$CONSUMER/.acr-canary/session-start.log"

mkdir -p -- "$CONSUMER/.acr-canary" &&
if [ -e "$SHARED" ]; then
  cp -- "$SHARED" "$EVIDENCE/$AGENT.before.log"
else
  printf 'no prior log to archive\n' > "$EVIDENCE/$AGENT.before.absent"
fi &&
: > "$SHARED" &&
wc -l < "$SHARED"          # before, must print 0
# start the agent, from "$CONSUMER", then:
wc -l < "$SHARED"          # after, must be greater than before
grep -c -- ACR-CANARY-SESSION-START-7f3a "$SHARED"   # must be at least 1
```

The `&&` chain is the archival guard: a `cp` that fails stops the sequence
before the truncation, because a prior log that could not be preserved must not
be destroyed to make room for this observation. Record the `before` line, and
the absence file when there was nothing to archive.

The row passes only when `before` is `0`, `after` is **strictly greater** than
`before`, and the lines that appeared are marker lines. Verified in scratch:
from an empty log one dispatch takes the count `0` → `1`, and reading the same
log twice without a second dispatch leaves it `1` → `1`. That second reading is
exactly the false pass this procedure exists to prevent — a presence-only check
accepts it.

**None of these establishes native dispatch:** a marker that was already there,
a line another agent's session wrote, a hook registration in a configuration
file, or running `session-start.sh` yourself. Only a line that appeared in this
agent's own empty log, after this agent's session started, does.

**Start each observation below in a clean session.** A rule quoted from an
earlier turn's memory is not a rule that loaded.

### Observations, and how each one is captured

For each agent — Claude Code, Codex, Cursor — record every applicable row, the
agent's build or version string, any trust or permission prompt verbatim, and an
identifier for each session used. A row with no recorded observation is not a
pass.

**The prompts must not give the answer away.** Never write a sentinel value into
a prompt, never name the fixture's files or directories, and never paste fixture
content into the conversation. Ask for an identifier the agent can only have if
the rule reached it.

**A tool-assisted answer is not a loading observation.** For the two rule rows,
the first turn must be tool-free: ask the agent not to open, read, list or
search any file, and not to use any tool. Capture whatever trace of tool use and
attachments the runtime already provides — a transcript export, a visible list
of tool calls, the attachment chips in its composer. If the runtime exposes none
of that, so you cannot tell a loaded rule from a file the agent read, the row is
**blocked**, not a pass. Do not invent a diagnostic flag or UI the runtime does
not have. An answer that came from a file search is **invalid** — record it as
such; it is not a native-loading pass and not a failure of the product either.

| Row | Applies to | Prompt and capture | Fails when |
| --- | --- | --- | --- |
| Always-on rule loaded | all three | Clean session, no file attached. First turn, tool-free: ask the agent to list every project rule currently in its context and quote each one's sentinel identifier line verbatim. Save the reply and the tool/attachment trace | The always-on sentinel is absent from a tool-free first turn |
| Skill is discoverable | all three | Clean session. First turn, tool-free: ask the agent to list the skills available to it in this project by name. Save the listing. **Discovery is established here, before anything is invoked** | The canary skill's name is absent from the listing, or the listing was produced by searching files |
| Skill executes | all three | Same session, after discovery: invoke the skill by the name the listing gave. Save the reply. A native skill invocation legitimately reads its own `SKILL.md` — that is activation, not a file search, and it does not invalidate this row | The invocation does not produce the skill's sentinel |
| Session-start hook dispatched | all three | The per-agent log procedure under **Session hygiene** — empty log proven before, at least one new marker line after. Save the before line, the after count and the log file | The log is absent or unchanged. A registration, a pre-existing marker, another agent's line, or a hook you ran yourself never counts |
| Scoped rule attaches only in its glob | **Cursor only** | Two separate clean sessions. In the first, deliberately attach only `docs/canary-in-scope.md`; in the second, only `canary-out-of-scope.md`. Same tool-free prompt in each: list the rules attached to this conversation and quote their sentinel identifier lines. Save both replies and both attachment traces | The scoped sentinel is missing in the in-glob session, or present in the out-of-glob one. If the runtime will not show what was attached, the row is **blocked** |

The last row is Cursor's alone because ACR's Cursor adapter is the only one that
realizes path scoping: it writes `.cursor/rules/acr__acr__canary__scoped.mdc`
with `globs: ["docs/**"]` and `alwaysApply: false` in its own file. ACR's Claude
Code and Codex adapters realize both canary rules into the shared always-on host
instead, so `AGENTS.md` and `CLAUDE.md` carry the scoped sentinel too and it is
expected in every session. Migration says so out loud —
`NOTE lossy: acr/canary/rule/scoped; applyTo prose clause`. The row is about
what ACR wrote, not about what those runtimes can do: nothing in this fixture
asks either of them to scope anything, so an out-of-glob absence would be a
control over an instruction that was never realized. Mark the row
**not applicable** and say why.

Classify every row **pass**, **fail**, **invalid** (the answer came from a file
search or a contaminated session — rerun it), **blocked** (the runtime cannot
expose the evidence, or the operator has no licence or runtime access — state
the reason), or **not applicable** (the adapter does not implement the feature).
"No error appeared" is not an observation, an empty cell is not a pass, and a
silently omitted row is a missing result rather than a passing one.
