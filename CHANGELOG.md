# Changelog

## Next

### Added

- Add the configurable `outdated`, `install`, and `none` session-start freshness policy, synthetic cross-agent hook realization, canonical 24-hour throttle state and project lock, fail-open CLI notices, and the rollback-hold seam for issue #16.
- Add `acr publish`, deterministic tagged-tree archives, schema-versioned release metadata, checksums, adapter realization gates, immutable draft-first GitHub Release uploads, consumer metadata verification, and a reusable publishing workflow for issue #9.
- Add production Claude Code, Codex, and Cursor adapters, persisted adapter selection, native hook/rule/skill/script layouts with invocation-safe skill paths, preservation-aware `realize`/`check` wiring, and cross-agent lifecycle goldens for issue #12.
- Add preservation-safe instruction include management for issue #6: deterministic include-graph discovery, byte-spliced managed Markdown blocks, surgical JSON/TOML merges, content-based ownership classification, and proof-bound promotion, demotion, and removal.
- Define the v1 `agent-plugin.yaml` contract, deterministic validator, package file enumeration, JSON Schema, and complete and minimal examples for issue #4.
- Add the `acr` command framework, stable process and output contracts, CLI reference, and macOS/Linux cross-build matrix for issue #13.
- Add deterministic `agents.yaml` dependency declarations, immutable registry locks, authenticated GitHub release/commit resolution, archive validation and content hashing, and install/list/outdated/update operations for issue #5.
- Add the adapter-neutral transactional realization planner, versioned ownership ledger, rollback-safe apply/check modes, and ownership-aware local Git exclusion management for issue #7.
- Add the versioned `internal/adapter` boundary: the read-only `Adapter` interface, data-only `Output` kinds, `compileOutputs` as the sole trusted bridge to `realize.Intent`, the capability preflight (`unsupported_adapter_capability`), the `Coordinator` library, and the `internal/adaptertest` golden-fixture harness with a reference fixture adapter, for issue #10.

### Fixed

- Reject symlinked manifests and Windows-prefixed artifact paths before reading package content, keep rule glob validation aligned with JSON Schema, and avoid derived repository diagnostics for invalid package names in issue #21.
- Reject non-canonical GitHub source URLs independently of package-name validation in issue #23.
