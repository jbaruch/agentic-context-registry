# Changelog

## Next

### Added

- Define the v1 `agent-plugin.yaml` contract, deterministic validator, package file enumeration, JSON Schema, and complete and minimal examples for issue #4.

### Fixed

- Reject symlinked manifests and Windows-prefixed artifact paths before reading package content, keep rule glob validation aligned with JSON Schema, and avoid derived repository diagnostics for invalid package names in issue #21.
