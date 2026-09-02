# Adapter boundary

`internal/adapter` is the versioned boundary between native agent renderers and the transactional realization engine described in [`docs/realization.md`](realization.md). An adapter inspects resolved packages and the project tree and renders data-only `Output` values; it never writes files and never constructs a `realize.Intent` directly. `compileOutputs` is the only trusted bridge from adapter output to `realize.Intent`.

This package defines the generic contract, the compilation guard, and a `Coordinator` library that resolves a fixed set of adapters against resolved packages. The shipped native implementations are `claude-code`, `codex`, and `cursor`, each at adapter version `1.0.0` and boundary version `1`. `internal/adaptertest` supplies shared golden fixtures plus reference and hostile adapters used to exercise the boundary end to end.

## Native adapters

For a package source `github:owner/repo`, every native artifact name is `acr__owner__repo__<artifact-id>`. Generated trees contain regular files only. Standalone scripts and hooks use mode `0755`; ordinary rule and skill files use `0644`, while executable files inside a skill retain `0755`.

| Artifact | Claude Code | Codex | Cursor |
| --- | --- | --- | --- |
| Rules | Shared `CLAUDE.md`/`AGENTS.md` managed block | Shared `AGENTS.md` managed block | `.cursor/rules/<name>.mdc` |
| Skills | `.claude/skills/<name>/**` | `.codex/skills/<name>/**` | `.cursor/skills/<name>/**` |
| Scripts | `.claude/scripts/<name>/<basename>` | `.codex/scripts/<name>/<basename>` | `.cursor/scripts/<name>/<basename>` |
| Hook executable | `.claude/hooks/<name>/<basename>` | `.codex/hooks/<name>/<basename>` | `.cursor/hooks/<name>/<basename>` |
| Hook configuration | `.claude/settings.json` | `.codex/config.toml` | `.cursor/hooks.json` |

Claude and Codex rule bundles reuse the preservation compiler's include graph. If `CLAUDE.md` imports `AGENTS.md`, both selections contribute one package block to the shared `AGENTS.md` host; rule bodies are not duplicated. Path-activated rules include their declared globs in the block. Cursor emits one `.mdc` per rule with exactly one leading frontmatter document: `alwaysApply: true` for unconditional rules, or `globs` plus `alwaysApply: false` for path activation. Any source frontmatter is parsed and discarded so the manifest remains authoritative.

The neutral hook vocabulary maps as follows:

| Manifest event | Claude Code | Codex | Cursor |
| --- | --- | --- | --- |
| `session-start` | `SessionStart` | `SessionStart` | `sessionStart` |
| `session-end` | `SessionEnd` | `SessionEnd` | `sessionEnd` |
| `user-prompt-submit` | `UserPromptSubmit` | `UserPromptSubmit` | `beforeSubmitPrompt` |
| `pre-tool-use` | `PreToolUse` | `PreToolUse` | `preToolUse` |
| `post-tool-use` | `PostToolUse` | `PostToolUse` | `postToolUse` |
| `stop` | `Stop` | `Stop` | `stop` |

Hook entries are merged structurally. Claude uses command matcher groups with `${CLAUDE_PROJECT_DIR}` and a separate `args` array. Codex emits native `[[hooks.<Event>]]` and `[[hooks.<Event>.hooks]]` tables, resolves the Git root in its command, and leaves `hooks.state` trust data untouched. Cursor uses command objects under `hooks.<event>`; it adds an owned root `version: 1` when missing, re-plans that field while the previous ledger records ACR ownership, preserves an existing unowned `version: 1`, and rejects any other schema value.

Each adapter validates its complete candidate projection before the engine runs. Stable native error codes cover event spelling, duplicate configuration, malformed Cursor frontmatter, incomplete or unsafe skill trees, and non-executable scripts/hooks. Detection is read-only and reports sorted evidence from the agent's instruction, configuration, rule, and skill paths.

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

### Snapshot implementations

`RootSnapshot` (`NewRootSnapshot(dir)`) is the production-safe `Snapshot`: it is backed by `os.OpenRoot`, so no path component and no symlink it follows can resolve outside the project directory — the same confinement `internal/realize`'s own write boundary relies on — and it rejects symlinks/special files at the leaf and caps read size. `FSSnapshot` (`NewFSSnapshot(fsys)`) is test-only scaffolding backed by any `fs.FS`; an `fs.FS` such as `os.DirFS` is explicitly permitted by its own contract to follow a symlink outside its root, so `FSSnapshot` must never back a real project tree.

## Capability preflight

`Coordinator.Realize` checks every `(adapter, source, artifact ID, artifact kind, hook event)` implied by the resolved packages against every selected adapter's `SupportedArtifacts()`/`SupportedEvents()` before calling any adapter's `Plan`. Any miss produces a sorted `*UnsupportedError` (code `unsupported_adapter_capability`) naming every combination; the preflight itself only calls `Descriptor`, `SupportedArtifacts`, and `SupportedEvents` — `Plan`, `Render`, and `Validate` never run, and no files change. The package manifest's own `unsupported_hook_event` (see [`docs/package-manifest.md`](package-manifest.md)) is a different check: it rejects event names outside the neutral v1 vocabulary, while `UnsupportedError` rejects a valid event the *selected adapter* cannot realize.

## Output kinds

An adapter never supplies a whole shared document; it supplies only the pieces the trusted compiler needs to derive one safely:

| Kind | Adapter supplies | Ledger `artifactKind` |
| --- | --- | --- |
| `generated-file` | Whole file content | `file` |
| `markdown-include` | One include/block body per insertion | `managed-block` |
| `config-merge` | Structural `(container, kind, key)` entries | `structured-entry` |

`compileOutputs` (`internal/adapter/compile.go`) groups every adapter's outputs by native target and is the sole place `Content`, `ObservedHash`, `ManagedIntact`, and `PreservedContent` are set on a `realize.Intent`:

- A `generated-file` output is rejected outright for an existing target the previous ledger does not already record as `generated-only` — a whole-file adapter output can never replace shared or unproven content. Promoting a changed generated file to shared ownership is only reachable through a `markdown-include`/`config-merge` output.
- A `markdown-include` or `config-merge` output is compiled through a registered `SharedCompiler`; without one, both kinds fail closed rather than silently skipping preservation.
- Duplicate managed-block IDs and duplicate `(container, kind, key)` structural entries across every contributing adapter and package are rejected as `*DuplicateEntryError` (code `duplicate_config_entry`); multiple adapters may otherwise contribute to the same shared target. Duplicate detection and sort ordering both use one length-prefixed encoding of every container segment, the entry kind, and the key, so no separator byte inside a segment can make two structurally different tuples collide.
- A `config-merge` output with a non-nil `Config` but zero `Entries` is rejected outright: it would otherwise render zero plan/render-correspondence tuples, letting an adapter with a completely empty plan pass correspondence trivially (0 == 0) while still contributing to compilation for any target it names — all while its own `Validate` never receives it. An adapter with nothing to contribute must omit the output entirely.
- `compileOutputs` also visits every previously shared target that has no current output at all (a package or artifact was removed), so the compiler can express a safe partial or final removal instead of the plain generated-only delete path, which would silently drop any surviving unmanaged or still-owned content.
- Every output's target is rejected by `realize.ValidateTargetPath` (reserved paths, parent traversal, absolute paths) before anything else runs — defense in depth alongside `realize.Planner`'s own authoritative rejection of the same paths on any intent, whatever produced it.

### The SharedCompiler seam

```go
type SharedCompiler interface {
	CompileMarkdown(ctx context.Context, request MarkdownCompileRequest) (SharedCompilation, error)
	CompileConfig(ctx context.Context, request ConfigCompileRequest) (SharedCompilation, error)
}
```

For Claude Code or Codex rule bundles, the shared compiler must also implement the `IncludeGraphProvider.DiscoverIncludeGraph(project, selectedRoots)` seam. The provider returns a read-only `InstructionIncludeGraph` used to validate the selected roots and choose their deepest shared host; an `invalid_include`, `unresolved_include`, `include_cycle`, or `duplicate_include` in a selected root's connected component aborts `Coordinator.Realize` and therefore both `acr realize` and `acr check`, while findings in untouched components remain warnings. Discovery indexes modes across the project but reads content only for roots and reachable include targets.

`compileOutputs` reconciles the previous ledger `Target` (if any) against this run's desired insertions/entries — which are empty for a no-current-output revisit — and passes both to the compiler as `SharedTarget{Path, Observed, Previous, ExplicitDemotion, Force}`; `Observed`/`Previous` are `nil` when the native file or the ledger target is absent. `ExplicitDemotion` and `Force` are coordinator/user options, never adapter data: `Coordinator.Realize` and `compileOutputs` both take a variadic, optional trailing `map[string]TargetOptions` keyed by native target path (omit it entirely for the default of no per-target overrides), and stamp `SharedTarget.ExplicitDemotion`/`Force` from the entry for that target. The compiler answers with one `SharedCompilation{Action, Candidate, Managed, Proof, Notices}`: `Action` becomes the `realize.Intent`'s action; `Candidate` (nil only for a proven whole-target removal) supplies `Content`/`Mode`/`Ownership`; `Managed` is stamped into ledger `Entry` values using the registered descriptor that contributed each entry's owner, never compiler or adapter data; `Proof` is copied verbatim onto `ObservedHash`/`ManagedIntact`/`PreservedContent`. A `ConfigEntry` with `Kind: ConfigElement` names its array by `Container` alone; the compiler locates the previous element by its ledger `managedHash`, never by array position, so an array reordered by something else still resolves correctly.

Each `ManagedResult` must set `Identity` to the contribution-level identity of the `Desired` entry it confirms — a `MarkdownInsertion`'s own `BlockID`, or `CanonicalEntryKey(entry.Container, entry.Kind, entry.Key)` for a `ConfigEntry` — never just `Owner`. Two different adapters can legitimately contribute the same `OwnerRef` (the same package artifact) to one target under distinct block IDs or config keys; `compileOutputs` matches each `ManagedResult` back to the descriptor that contributed it by `Identity`, since `Owner` alone cannot tell them apart and duplicate detection already guarantees `Identity` is unique across every adapter for that target.

A config-merge target's structural format (JSON or TOML) is never guessed from its file extension — a caller-controlled path is not trustworthy evidence. When a current adapter output declares `Config.Format`, that declaration is used (and rejected as malformed if a caller-supplied `TargetOptions.ConfigFormat` disagrees with it). When `compileOutputs` revisits a target with no current output at all, `TargetOptions.ConfigFormat` is the only source of truth; without it, compilation fails closed rather than inferring anything from the path.

`internal/preserve` supplies the production `SharedCompiler` from issue #6: concrete JSON/TOML/Markdown preservation, marker grammar, include-graph discovery, and promotion/demotion mechanics. `internal/adapter` defines only the seam and its fail-closed guard.

## Engine defense in depth

The realization planner's `preserves()` check is vacuously true when an intent's `PreservedContent` is empty, so a hand-built intent that otherwise satisfies every other merge precondition could still whole-file-replace a shared target. `internal/realize`'s planner closes this independently of the adapter boundary: a shared `ensure` or `promote` whose observed file is non-empty and whose `PreservedContent` carries no non-empty fragment is a conflict, whether or not the intent reached the planner through `compileOutputs`.

## Coordinator

`Coordinator.Realize(ctx, project, packages, previous)` runs the complete boundary as a library, without ever calling `realize.Engine` itself:

1. The capability preflight (above).
2. Each registered adapter's `Plan` then `Render`, in adapter-ID order.
3. For each adapter, an exact correspondence check between its `NativePlan.Items` and its own rendered contributions — every `(target, kind, mode, owner)` tuple the plan promises must appear in the render exactly as often, and vice versa, and `NativePlan.Adapter` must match the registered descriptor. A plan with no items can therefore never smuggle an arbitrary rendered output through compilation, and one adapter's output can never mask another adapter's planned-but-never-rendered target, even when both legitimately share one native file.
4. `compileOutputs` over every adapter's rendered `Output` values.
5. Each adapter's `Validate` over the compiled `CandidateFile`s for its own planned targets.

It returns `realize.Intent` values only when every stage succeeds. A caller must never invoke `realize.Engine.Run(ModeApply, ...)` when `Realize` returns an error — the returned intent slice is `nil` in that case, so there is nothing to apply. `internal/realizeapp` selects persisted or flag-supplied adapters, materializes and verifies immutable locks, invokes this coordinator, and runs the transactional engine for `acr realize` and `acr check`.

`ctx` is threaded verbatim into every `Plan`/`Render`/`Validate` call and into `compileOutputs`'s own `SharedCompiler` calls — never replaced with `context.Background()` — so a caller's cancellation or deadline actually reaches the compiler. `Realize` drops any `SharedCompilation.Notices` a compiler returned; `RealizeWithNotices` returns the identical result plus every notice gathered across all compiled targets, in target-path order — the way a caller learns about #6's `shared_file_requires_commit` promotion notice, for example.

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

`RunGoldenFixture` supports a shared package/project fixture with separate adapter-specific `want/` trees. The checked-in `all-agents` and `freshness-session-start` matrices exercise Claude Code, Codex, and Cursor against identical inputs, including modes, preservation, idempotence, cleanup, and event vocabulary.

`internal/adaptertest.NewReferenceAdapter` is a fixture/hostile test double, not a production adapter.
