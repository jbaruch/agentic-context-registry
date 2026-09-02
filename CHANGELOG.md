# Changelog

## Next

### Added

- Add `acr migrate tessl --dry-run` Tessl inventory for issue #1: read-only classification of installed plugin.json and tile.json packages onto the logical artifact model, preserved unmanaged spans, and a schema-versioned text/JSON report.
- Add temporary rollback holds for `latest` dependencies for issue #17: a reviewable `hold` on the declaration (known-good pin, rejected release, optional reason) and the rejected release's resolved identity on the lock; the real `HoldPolicy` filling #16's seam, which `acr install`, `acr update`, and the session-start `install` policy all consult so none can reinstall a rejected release; the `--hold` / `--pin` downgrade choice, required for every explicit reference that does not move a `latest` declaration forward, equality included, with a typed `downgrade_choice_required` usage error when neither is supplied and neither flag accepted over a standing hold unless the reference is proven not to advance the held release; `acr resume SOURCE` with `--dry-run` as the only path back to `latest`, alongside the explicit `--pin` conversion; `update` / `held` / `beyond-barrier` classification in `acr outdated` with held rows suppressed at session start; and a schema bump to version 2 with in-memory upgrade from version 1, with a hold recorded under version 1 refused by the runtime and by both JSON Schemas, so an older binary refuses a held file instead of silently dropping the hold.
- Add deterministic signed `acr` binaries for macOS and Linux, release version and source-commit reporting, checksums, CycloneDX SBOM and provenance verification, immutable tag release automation, tested Homebrew distribution, and verified install documentation for issue #15.
- Add the configurable `outdated`, `install`, and `none` session-start freshness policy, synthetic cross-agent hook realization, canonical 24-hour throttle state and project lock, fail-open CLI notices, and the rollback-hold seam for issue #16.
- Add `acr publish`, deterministic tagged-tree archives, schema-versioned release metadata, checksums, adapter realization gates, distinct Git-access failure codes, immutable draft-first GitHub Release uploads, consumer metadata verification, and a reusable publishing workflow for issue #9.
- Add `acr migrate tessl-plugin` to convert Tessl `tile.json` and `.tessl-plugin/plugin.json` packages into `agent-plugin.yaml` without rewriting artifact source files, for issue #11.
- Add production Claude Code, Codex, and Cursor adapters, persisted adapter selection, native hook/rule/skill/script layouts with invocation-safe skill paths, preservation-aware `realize`/`check` wiring, and cross-agent lifecycle goldens for issue #12.
- Add preservation-safe instruction include management for issue #6: deterministic include-graph discovery, byte-spliced managed Markdown blocks, surgical JSON/TOML merges, content-based ownership classification, and proof-bound promotion, demotion, and removal.
- Define the v1 `agent-plugin.yaml` contract, deterministic validator, package file enumeration, JSON Schema, and complete and minimal examples for issue #4.
- Add the `acr` command framework, stable process and output contracts, CLI reference, and macOS/Linux cross-build matrix for issue #13.
- Add deterministic `agents.yaml` dependency declarations, immutable registry locks, authenticated GitHub release/commit resolution, archive validation and content hashing, and install/list/outdated/update operations for issue #5.
- Add the adapter-neutral transactional realization planner, versioned ownership ledger, rollback-safe apply/check modes, and ownership-aware local Git exclusion management for issue #7.
- Add the versioned `internal/adapter` boundary: the read-only `Adapter` interface, data-only `Output` kinds, `compileOutputs` as the sole trusted bridge to `realize.Intent`, the capability preflight (`unsupported_adapter_capability`), the `Coordinator` library, and the `internal/adaptertest` golden-fixture harness with a reference fixture adapter, for issue #10.

### Fixed

- Fail `acr migrate tessl --dry-run` when tessl.json names a dependency that is not a two-segment workspace/package identity, for issue #1.
- Fail `acr migrate tessl --dry-run` when `.tessl/RULES.md` includes a non-package-relative rules path, and mark two plugin.json declarations that share an artifact ID as ambiguous, for issue #1.
- Fail `acr migrate tessl --dry-run` when a Tessl manifest declares a non-package-relative rule or skill path, or when a native JSON/TOML config cannot be decoded, instead of reading outside the package or omitting user content, for issue #1.
- Fail `acr migrate tessl --dry-run` when a declared skill's `SKILL.md` exists but cannot be read, instead of classifying it as `missing-skill`, for issue #1.
- Recognize the Tessl hook dispatcher only at the head of the command in the issue #1 inventory, so a user hook that quotes `tessl hook run` in an argument or a shell comment stays preserved.
- Report orphan Tessl native files under adapter rule and skill roots as `unmapped` with reason `orphan-tessl-native` in the issue #1 inventory, instead of leaving a partial uninstall or stale native tree silent.
- Keep a Tessl-declared skill in the issue #1 inventory when its `SKILL.md` is absent, instead of expanding sibling directories as phantom skills.
- Treat a user hook that names a `.tessl/` path as preserved in the issue #1 inventory; Tessl ownership of native hook commands is the dispatcher literal, not a path substring.
- Keep Tessl package files and `.tessl/RULES.md` out of `preserved` so issue #1's inventory reports them as artifacts or unmapped, never as unmanaged user content.
- Fail `acr migrate tessl --dry-run` when the include-graph snapshot cannot be read instead of printing a partial inventory for issue #1.
- Print Tessl inventory failures with a single CLI recovery sentence for issue #1.
- Treat a rule declared by only one of `plugin.json` and a stale `tile.json` as migratable, not ambiguous, in the issue #1 inventory.
- Reject malformed Tessl hook commands when computing plugin-tree ownership so they cannot hide undeclared files from `unmapped` in issue #1.
- Keep an unsupported duplicate Tessl skill classified as unsupported rather than downgrading it to ambiguous in issue #1.
- Report symlinked entries under `.tessl/plugins/**` as `unmapped` with reason `plugin-symlink` so issue #8 can vendor them.
- Omit the always-empty per-package `unmapped` group from the issue #1 Tessl inventory text report.
- Classify em-dash-free `applyTo:` on always-on rules as prose instead of globs for issue #11.
- Include populated unmapped reports in producer-migration JSON and text refusals for issue #11.
- Preserve scalar fields declared by only one Tessl manifest, including tile-only description, repository, and provenance, for issue #11.
- Detect `nativeHooks` agent widening by adapter membership rather than set size, so a three-agent declaration that omits an ACR adapter is `agent_widening`, for issue #11.
- Refuse conversion when tile.json and plugin.json disagree on `private`, and treat `private: true` on either manifest as `unmapped_field`, for issue #11.
- Treat a `rules` or `skills` set declared by only one Tessl manifest as `ambiguous_manifest` instead of dropping the other side's artifacts, for issue #11.
- Report both halves of an `alwaysApply: true` plus `applyTo:` rule, surface the refused Tessl field on `--json` errors, and reject parent-directory segments in declared rule paths as `invalid_path`, for issue #11.
- Report the invocation's dry-run flag on producer-migration JSON refusals for issue #11.
- Emit `ignored` as an empty array rather than null on successful producer-migration JSON for issue #11.
- Write producer-migration manifests through a package-local temporary file and rename, so a failed write cannot leave a partial `agent-plugin.yaml` that later reports `manifest_conflict`, for issue #11.
- Route malformed or unreachable project state through freshness fail-open attempts that record throttle state, keep JSON application errors to one parseable error object when notices accompany them, document future-timestamp recovery, and preserve a re-declared dependency's current requested policy when a rollback hold keeps its existing lock in issue #35.
- Reject symlinked manifests and Windows-prefixed artifact paths before reading package content, keep rule glob validation aligned with JSON Schema, and avoid derived repository diagnostics for invalid package names in issue #21.
- Reject non-canonical GitHub source URLs independently of package-name validation in issue #23.
