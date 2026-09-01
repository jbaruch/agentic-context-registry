# Changelog

## Next

### Added

- Define the v1 `agent-plugin.yaml` contract, deterministic validator, package file enumeration, JSON Schema, and complete and minimal examples for issue #4.
- Add the `acr` command framework, stable process and output contracts, CLI reference, and macOS/Linux cross-build matrix for issue #13.
- Add deterministic `agents.yaml` dependency declarations, immutable registry locks, authenticated GitHub release/commit resolution, archive validation and content hashing, and install/list/outdated/update operations for issue #5.

### Fixed

- Reject symlinked manifests and Windows-prefixed artifact paths before reading package content, keep rule glob validation aligned with JSON Schema, and avoid derived repository diagnostics for invalid package names in issue #21.
- Reject non-canonical GitHub source URLs independently of package-name validation in issue #23.
