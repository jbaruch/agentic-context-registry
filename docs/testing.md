# Testing

This page describes how the project proves the CLI works, what each lane can
and cannot establish, and where the remaining boundaries are.

## Gates

`CONTRIBUTING.md` names the four commands every change runs locally, and CI
runs the same four on Ubuntu and macOS:

| Gate | Command |
| --- | --- |
| Formatting | `test -z "$(gofmt -l .)"` |
| Diagnostics | `go vet ./...` |
| Tests | `go test -race ./...` |
| Build | `go build ./cmd/acr` |

The acceptance journeys run inside the ordinary `go test -race ./...` gate. No
journey reaches an external service, uses an operator credential, sleeps, or
reads the wall clock.

## Lanes

Each lane proves a different thing, and one never stands in for another.

| Lane | What runs | What it establishes |
| --- | --- | --- |
| Component | A package's own tests | Combinatorial edge cases, error shapes, parsers |
| Composed, in process | `cmd/acr`'s `runWith` with the production `GitHubClient` over a fixture transport | The real parser, composition, resolver, adapters and transactional engine, end to end |
| Composed subprocess | The same `runWith` in a separate process, from a test entry point | Real file descriptors, argv, exit status and stream separation |
| Shipped binary | The compiled `cmd/acr` executable | What a user's shell gets, for local and offline behavior |

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

State advances by running commands. A publish journey creates the release its
consumer then installs — nothing seeds the outcome it is supposed to prove.

## Command journey map

`TestCLIJourneyInventory` in `cmd/acr` runs one table of journeys and then
holds the result against `cli.Leaves()`, the production inventory the parser
dispatches on and help renders. Every executable leaf needs both a successful
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
| `freshness run` | `policies-and-throttle` | Each policy's effect, the generated wrapper, the throttled second attempt |
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

`TestCLIJourneyInventoryFailsForTheIntendedCause` mutates the inventory and the
evidence — a new command, a new subcommand, a removed journey, a leaf that only
refuses, a leaf that only succeeds, a journey with no runnable function — and
requires the gate to complain by name for each.

## Boundaries

These are the things the deterministic suite does **not** establish. They are
recorded here rather than implied to be covered.

- **Live GitHub conformance.** Every remote journey answers from a fixture. A
  fixture can agree with a mistaken model of the API, and the shipped binary's
  successful network path is not exercised at all. The intended remedy is a
  separate scheduled, read-only workflow that runs the built binary against an
  owner-designated immutable public fixture repository, plus an explicitly
  opt-in publisher smoke against an owner sandbox repository. Neither exists
  yet, and this round authorized no external write.
- **Real agent behavior.** The journeys assert the bytes, modes and native
  configuration entries each adapter writes. They do not establish that Claude
  Code, Codex or Cursor loads a realized rule, discovers a skill, honors trust
  settings, or dispatches the session-start hook. Only a pinned real-runtime
  smoke with agent-side diagnostics can show that.
- **Fixture provenance.** Package and response fixtures are generated from text
  in the test files, so they are inspectable and diffable, but they were
  written from the API contract rather than captured from a recorded live
  exchange. A capture-and-refresh procedure — endpoint, repository, commit,
  observed date, headers, ordered tar entries, sanitation applied — belongs
  beside the builders when the live conformance lane lands, on a stated refresh
  cadence and through a reviewed diff. Expected data is never regenerated from
  the function under test, and a failure is never made green by refreshing it.
- **Filesystem permission failures.** Refusals are proven for unreachable
  sources, tampered archives, edited outputs and malformed state. A
  non-writable destination as an unprivileged user is not yet a journey.
