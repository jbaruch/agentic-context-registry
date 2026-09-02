# Freshness state schema

`internal/freshness` owns and writes the per-machine throttle record. The session-start freshness runner reads and rewrites it; `ReadState` is the read-only status-display seam and never migrates it.

Schema version 1 is one JSON object with fields in this order:

- `schemaVersion`: integer `1`.
- `project`: `sha256:` plus the canonical project identity digest.
- `lastCheckedAt`: an RFC 3339 UTC timestamp.
- `lastPolicy`: `outdated` or `install`.
- `lastOutcome`: `ok`, `offline`, `auth`, `failed`, or `conflict`.

Missing, corrupt, invalid, older, or newer records are no usable prior state. The owning runner replaces them with schema version 1 after its next attempted check.
