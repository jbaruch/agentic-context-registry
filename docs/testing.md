# Testing

This page describes how the project proves the CLI works, what each lane can
and cannot establish, and where the remaining boundaries are.

## Gates

`CONTRIBUTING.md` names the four commands every change runs locally. CI runs
formatting and diagnostics once on Ubuntu, and the test and build gates on both
Ubuntu and macOS:

| Gate | Command |
| --- | --- |
| Formatting | `test -z "$(gofmt -l .)"` |
| Diagnostics | `go vet ./...` |
| Tests | `go test -race ./...` |
| Build | `go build ./cmd/acr` |

The acceptance journeys run inside the ordinary `go test -race ./...` gate. No
journey reaches an external service, uses an operator credential, or sleeps.

No journey asserts anything about wall-clock time. Where elapsed time is the
subject — the freshness throttle window — the journey injects its own clock
through `freshnessapp.WithClock` and moves it on purpose, so the window is
crossed because the journey crossed it. Everywhere else the composed stack
reads `time.Now` exactly as the shipped binary does, and no assertion depends
on what it returns. Git fixtures commit at fixed author and committer dates.

## Lanes

Each lane proves a different thing, and one never stands in for another.

| Lane | What runs | What it establishes |
| --- | --- | --- |
| Component | A package's own tests | Combinatorial edge cases, error shapes, parsers, and the transaction and rollback faults the journeys do not re-run |
| Composed, in process | `cmd/acr`'s `runWith` with the production `GitHubClient` over a fixture transport | The real parser, composition, resolver, adapters and transactional engine, end to end |
| Composed subprocess | The same `runWith` in a separate process, from a test entry point | Real file descriptors, argv, exit status and stream separation |
| Shipped binary | The compiled `cmd/acr` executable | What a user's shell gets, for local and offline behavior |
| Codex runtime | `internal/producerconvert`'s `TestCodexRuntimeRealRelease` against one official Codex release inside the host's real read boundary, no credential | That the release parses ACR's argument vector, honors every control, starts inside the boundary and reaches the model service; the `codex-runtime` CI job runs it for every verified release on Ubuntu and macOS |
| Codex live | `cmd/acr`'s `TestCodexLive*` with `ACR_CODEX_LIVE=1` and a configured credential, then `TestSemanticLiveGeneratedPublication` on the converted commit | A real semantic conversion of the clean fixture and of the untouched upstream fixtures, the original upstream tests on the converted tree, and publication, installation, realization and check of that same commit through the GitHub fixture; hosted acceptance uses the central subscription route described below |

The two Codex lanes never run inside the ordinary `go test -race ./...` gate: the runtime lane needs a real release binary (`ACR_CODEX_RELEASE_BIN`) and the live lane needs `ACR_CODEX_LIVE=1`, a credential and an evidence directory. Each lane turns its skip into a failure under `ACR_CODEX_RELEASE_REQUIRED=1` or `ACR_CODEX_LIVE_REQUIRED=1`, which the CI jobs set, so a lane cannot go green by skipping. What does run in the ordinary gate on every platform is `TestCodexReadBoundaryDeniesInheritedInstructionsNatively`, which exercises the real Seatbelt profile on macOS and the real bubblewrap view on Linux and fails, never skips, when the mechanism is missing; the Linux test leg installs bubblewrap for it. Every other Codex test drives the production flow through scripted `codex`, `sandbox-exec` and `bwrap` fakes that reproduce the real wrappers' argument contracts, and the recorded startup streams under `internal/producerconvert/testdata/codex/` carry the provenance of every verified release.

The composed-subprocess lane is **not** shipped-binary coverage. Routing the
shipped executable at a local fixture would need a test-only switch in
production code, so the binary's successful network path is left to the live
conformance work described under [Boundaries](#boundaries).

## The GitHub fixture

`cmd/acr/journey_github_test.go` implements the GitHub REST boundary as a
stateful server: releases, commits, tag refs, source tarballs, asset uploads
and asset downloads, each answering from one store that every later request
observes. Requests keep their production hostnames — `api.github.com`,
`uploads.github.com`, `codeload.github.com`,
`objects.githubusercontent.com` — and a transport routes those names to the
fixture, so the client still builds, authenticates, validates and
redirect-checks exactly the requests it builds against GitHub itself. An
endpoint the fixture does not implement fails the test rather than passing
quietly.

Source tarballs carry what GitHub actually serves: a leading
`pax_global_header` entry holding the commit, one real root directory, and
group permissions (`0664` / `0775`) that no publisher records. Package fixtures
are generated from text during setup; no binary archive is committed.

Where that shape came from, which parts were observed from a real GitHub
tarball and which were written from the API contract, which hash means what, and
how to refresh any of it are in
[`cmd/acr/fixture-provenance.md`](../cmd/acr/fixture-provenance.md).

State advances by running commands. A publish journey creates the release its
consumer then installs — nothing seeds the outcome it is supposed to prove.

## Command journey map

`TestCLIJourneyInventory` in `cmd/acr` runs one table of journeys and then
holds the result against `cli.Leaves()`, which enumerates the dispatch registry
`commandFor` accepts rather than the display order, so a command registered
without one is a leaf the gate demands a journey for. Root help enumerates the
same registry. Every executable leaf needs both a successful
outcome and a refusal, and a journey registers coverage only after asserting a
positive result — a locked dependency, a written artifact, an applied update, a
removed target, a converted package. A command name alone, a help-only test, or
an empty result registers nothing.

| Leaf | Journey | Proves |
| --- | --- | --- |
| `init` | `detection-override-repeat`, `pty-selection-and-reprompt` | Persisted selection, detection, dry run, repeat, real terminal answers |
| `install` | `latest-tag-and-commit`, `reconcile-and-repeat`, `subprocess-streams-and-exits` | Immutable locks for every policy, moved latest, preserved pins, exits 0–4 |
| `realize` | `native-layouts-and-subset` | Claude, Codex and Cursor outputs, companions, modes, hook registration, subset scoping |
| `list` | `mixed-dependencies` | Latest, pinned and held rows in both formats, with no network |
| `outdated` | `current-newer-and-pinned` | Checked-current versus genuinely newer, no lookup for a pin |
| `freshness run` | `policies-and-throttle` | Each policy's effect, the generated wrapper, and an injected clock crossing Window-1s and Window |
| `update` | `target-all-and-dry-run` | Predicted then applied moves, scoped to eligible rows |
| `resume` | `barrier-lifecycle` | A hold surviving reconcile and update, and the one command that ends it |
| `uninstall` | `sibling-last-and-repeat` | Pruned outputs, surviving sibling and operator files, offline last removal |
| `check` | `current-drift-and-repair` | Current, drifted and repaired, with the affected path named |
| `publish` | `consumer-roundtrip` | Real tagged Git to draft, upload, readback, publication, then a fresh consumer install |
| `migrate tessl` | `mapped-vendor-and-finalize` | Mapping, vendoring, coexistence, then finalization |
| `migrate tessl-plugin` | `convert-publish-consume` | Conversion without repository metadata, then publish and consume |
| `help` | `aliases-and-commands` | Every leaf reachable and documented, aliases identical, nothing written |
| `version` | `aliases-and-json` | A supplied identity reported in text and one JSON envelope |

Each leaf also carries a refusal journey; `TestCLIJourneyInventory` fails if
either obligation is missing.

`TestCLIJourneyInventory` also runs the real gate, with the real evidence,
against the production surface with one command registered but never ordered or
covered, and requires the complaint to name it.
`TestCLIJourneyInventoryFailsForTheIntendedCause` mutates a synthetic inventory
the same ways — a new command, a new subcommand, a removed journey, a leaf that
only refuses, a leaf that only succeeds, a journey with no runnable function —
and requires the gate to complain by name for each.

## Boundaries

These are the things the deterministic suite does **not** establish. They are
recorded here rather than implied to be covered.

- **Live GitHub conformance.** Every remote journey answers from a fixture, and
  a fixture can agree with a mistaken model of the API. The shipped binary's
  successful network path is not exercised by any gate.
  [`manual-conformance.md`](manual-conformance.md) check 1 is the read-only
  procedure that covers it, run by hand on a stated cadence. Automating it as a
  scheduled workflow, and adding an opt-in publisher smoke against an owner
  sandbox repository, remain future scope; the publisher smoke writes, so it
  stays out until separately authorized.
- **Real agent behavior.** The journeys assert the bytes, modes and native
  configuration entries each adapter writes. They do not establish that Claude
  Code, Codex or Cursor loads a realized rule, discovers a skill, honors trust
  settings, or dispatches the session-start hook — a registered hook is not an
  executed one. [`manual-conformance.md`](manual-conformance.md) check 2 is the
  canary that observes it, per agent build: it installs the local
  [`canary-package/`](canary-package/) with no network and no publication, and
  `cmd/acr/canary_test.go` runs every phase of that recipe except starting the
  agents, so the manual half is the observation and not the setup.
- **Fixture provenance.** Response bodies were written from the API contract,
  not captured from a recorded exchange, so no deterministic test can notice a
  fixture that agrees with a mistaken model.
  [`cmd/acr/fixture-provenance.md`](../cmd/acr/fixture-provenance.md) separates
  the observed archive facts from the synthetic ones, names the fields that were
  not captured, and carries the refresh cadence and procedure.
- **Codex conversion at scale.** Both untouched fixtures are required in required mode; missing or `skip` fixture values fail. Original GOC commands cover 15 classification cases plus CLI checks, 16 scaffold checks, and 23 envelopes plus the negative path. FFA retains the original gate, at least 184 tests and Pyright 1.1.411. GH_TOKEN for original suites is separate from the Codex environment. Baselines are `tesslio/good-oss-citizen@f21fda887815af815979a4fea43a66eb5174ee3e` and `jbaruch/frequent-flyer-advocate@142babbb1e2bebc798eb42128ac2466f21b5131d`. Live success, real hosted publication, and local/CI consumption of one exact generated commit remain acceptance work; independently generated commits do not prove that obligation.

The central subscription acceptance implementation is maintained in [coding-policy](https://github.com/jbaruch/coding-policy/blob/main/docs/acr-codex-accept.md). It retains CODEX_AUTH_JSON centrally and uses ACR's existing FLEET_DISPATCH_TOKEN infrastructure. The earlier ACR-local API-only dispatch workflow has been removed. Registration, the exact reviewed central/candidate commits, authenticated conversion, publication and same-commit consumers must be confirmed from actual runs before claiming hosted acceptance. The auth-free six-entry runtime matrix and checksummed installer remain in this repository.

`TestCodexCredentialBoundaryRuntime` and `TestCodexCredentialBoundaryCLI` exercise synthetic credential rotation, proposal/plan refusal, cleanup, sanitation and native CLI composition. They establish deterministic guard behavior, not successful hosted model authentication.

- **Filesystem permission failures.** Refusals are proven for unreachable
  sources, tampered archives, edited outputs and malformed state. A
  non-writable destination as an unprivileged user is not yet a journey; the
  injected `os.ErrPermission` tests prove error propagation, not denial by the
  filesystem.
