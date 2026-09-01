# Adapter boundary

`internal/adapter` is the versioned boundary between native agent renderers and the transactional realization engine described in [`docs/realization.md`](realization.md). An adapter inspects resolved packages and the project tree and renders data-only `Output` values; it never writes files and never constructs a `realize.Intent` directly. `compileOutputs` is the only trusted bridge from adapter output to `realize.Intent`.

This package defines the generic contract, the compilation guard, and a `Coordinator` library that resolves a fixed set of adapters against resolved packages. It ships no native adapter: issue #12 adds the Claude Code, Codex, and Cursor implementations against this boundary. `internal/adaptertest` supplies a golden-fixture harness and a reference/hostile fixture adapter used to exercise the boundary end to end.

## Contract

`Descriptor{ID, Version, Boundary}` identifies an adapter implementation. `Boundary` is the exact API and semantic contract version (`CurrentBoundaryVersion`); a signature or semantic compatibility break to this package increments it, while an adapter's own output changes bump `Version` instead. `Register` validates every adapter before it can be coordinated: a unique, lowercase kebab-case ID; a valid semantic `Version`; sorted, duplicate-free `SupportedArtifacts()`/`SupportedEvents()`; and `Boundary == CurrentBoundaryVersion`, or an actionable `*BoundaryVersionError`.

The `Adapter` interface:

| Method | Purpose |
| --- | --- |
| `Descriptor()` | Identity and version, used for ledger stamping and diagnostics |
| `Detect(ctx, DetectRequest)` | Read-only project inspection for `acr init` preselection |
| `SupportedArtifacts()` / `SupportedEvents()` | The closed capability set the coordinator's preflight checks against |
| `Plan(ctx, PlanRequest)` | Maps resolved package artifacts onto native target paths and kinds, without bytes |
| `Render(ctx, RenderRequest)` | Produces the adapter's `Output` values for a `NativePlan` |
| `Validate(ctx, ValidateRequest)` | Adapter-owned semantic checks over compiled candidates, before intents reach `realize.Engine` |

## Capability preflight

`Coordinator.Realize` checks every `(adapter, source, artifact ID, artifact kind, hook event)` implied by the resolved packages against every selected adapter's `SupportedArtifacts()`/`SupportedEvents()` before calling any adapter's `Plan`. Any miss produces a sorted `*UnsupportedError` (code `unsupported_adapter_capability`) naming every combination; no adapter method runs and no files change. The package manifest's own `unsupported_hook_event` (see [`docs/package-manifest.md`](package-manifest.md)) is a different check: it rejects event names outside the neutral v1 vocabulary, while `UnsupportedError` rejects a valid event the *selected adapter* cannot realize.

## Output kinds

An adapter never supplies a whole shared document; it supplies only the pieces the trusted compiler needs to derive one safely:

| Kind | Adapter supplies | Ledger `artifactKind` |
| --- | --- | --- |
| `generated-file` | Whole file content | `file` |
| `markdown-include` | One include/block body per insertion | `managed-block` |
| `config-merge` | Structural `(container, kind, key)` entries | `structured-entry` |

`compileOutputs` (`internal/adapter/compile.go`) groups every adapter's outputs by native target and is the sole place `Content`, `ObservedHash`, `ManagedIntact`, and `PreservedContent` are set on a `realize.Intent`:

- A `generated-file` output is rejected outright for an existing target the previous ledger does not already record as `generated-only` — a whole-file adapter output can never replace shared or unproven content. Promoting a changed generated file to shared ownership is only reachable through a `markdown-include`/`config-merge` output.
- A `markdown-include` or `config-merge` output is compiled through a registered `SharedCompiler`; without one, both kinds fail closed rather than silently skipping preservation. The compiler's `MergedDocument` is the only source of `ObservedHash`/`ManagedIntact`/`PreservedContent` — adapters cannot set those fields themselves because `Output`, `MarkdownInsertion`, and `ConfigEntry` have no such fields to set.
- Duplicate managed-block IDs and duplicate `(container, kind, key)` structural entries across every contributing adapter and package are rejected as `*DuplicateEntryError` (code `duplicate_config_entry`); multiple adapters may otherwise contribute to the same shared target.

Issue #6 supplies the production `SharedCompiler` — concrete JSON/TOML/Markdown preservation, marker grammar, and promotion/demotion mechanics. This package defines only the seam and its fail-closed guard.

## Engine defense in depth

The realization planner's `preserves()` check is vacuously true when an intent's `PreservedContent` is empty, so a hand-built intent that otherwise satisfies every other merge precondition could still whole-file-replace a shared target. `internal/realize`'s planner closes this independently of the adapter boundary: a shared `ensure` or `promote` whose observed file is non-empty and whose `PreservedContent` carries no non-empty fragment is a conflict, whether or not the intent reached the planner through `compileOutputs`.

## Coordinator

`Coordinator.Realize(ctx, project, packages, previous)` runs the complete boundary as a library, without ever calling `realize.Engine` itself:

1. The capability preflight (above).
2. Each registered adapter's `Plan` then `Render`, in adapter-ID order.
3. `compileOutputs` over every adapter's rendered `Output` values.
4. Each adapter's `Validate` over the compiled `CandidateFile`s for its own planned targets.

It returns `realize.Intent` values only when every stage succeeds. A caller must never invoke `realize.Engine.Run(ModeApply, ...)` when `Realize` returns an error — the returned intent slice is `nil` in that case, so there is nothing to apply. Selecting which adapters a project targets, persisting that selection, and wiring `acr realize`/`acr check` to the coordinator are outside this boundary; issue #12 adds that orchestration.

An adapter version bump alone — identical rendered bytes, only `Descriptor.Version` changed — still regenerates every affected ledger entry, because `compileOutputs` stamps `Adapter`/`AdapterVersion` from the currently registered descriptor. The next plan is non-empty and reviewable (`ledgerChanged: true`, an `update` operation with equal before/after hashes) even though no bytes changed; no file is rewritten until `ModeApply`.

## Golden fixtures

`internal/adaptertest.RunGolden` drives an adapter and `SharedCompiler` through every case directory under `testdata/`:

```text
testdata/<case>/
  package/agent-plugin.yaml   # plus every declared artifact file
  project/                    # starting native files (omit for an empty project)
  want/plan.json              # success cases: the realize.Plan, content omitted by design
  want/files/<path>           # success cases: the complete resulting project tree
  want/error.json             # error cases only: {"error": "<exact Coordinator.Realize error>"}
```

`go test ./internal/adaptertest/... -run TestReferenceAdapterGolden -update` rewrites `want/` from the adapter's actual output; without `-update`, the harness compares the plan JSON and every realized file's bytes, and fails on any extra or missing file. Fixtures are UTF-8 text, built once and checked in, never generated from wall-clock time or unseeded randomness.

`internal/adaptertest.NewReferenceAdapter` is a fixture/hostile test double, not a production adapter — issue #12 ships the real Claude Code, Codex, and Cursor implementations against this same boundary.
