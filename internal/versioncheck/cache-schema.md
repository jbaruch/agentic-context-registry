# Version cache schema

`internal/versioncheck` owns and writes the machine-level release records. The
CLI entry point reads the published record before every eligible command and
rewrites both records after the command's output, at most once per 24-hour
window; nothing else writes them.

Both records live under `${ACR_STATE_HOME:-<user-cache>/acr}/version/`, beside
the advisory lock `version.lock` every refresh holds. Neither is keyed by a
project.

`latest.json` is the published record, schema version 1, one JSON object with
fields in this order:

- `schemaVersion`: integer `1`.
- `checkedAt`: an RFC 3339 UTC timestamp, when the release was observed.
- `latestVersion`: the latest stable release tag, a valid semantic version with
  no prerelease part.

`attempt.json` is the throttle bookkeeping, schema version 1:

- `schemaVersion`: integer `1`.
- `attemptedAt`: an RFC 3339 UTC timestamp, when a refresh was last attempted,
  whatever it returned.

A successful refresh rewrites both. A failed refresh rewrites `attempt.json`
alone, so the published bytes survive it. Missing, corrupt, oversized, older,
or newer records are no usable prior state: a missing or unusable published
record is silent, and a missing or unusable attempt record makes the next
refresh due. The owning refresh replaces them with schema version 1 after its
next attempt.

Every read and write goes through a descriptor to the `version` directory
whose identity is verified after it is opened, and every entry beneath it is
classified from its own metadata before it is opened. A symlink where the
directory belongs, whether it escapes the store or points back inside it, is
refused before any record is read, created or renamed, and the directory is
opened as an intermediate path element so an entry swapped in after its
inspection — a named pipe, a file — is refused without a blocking open. A symlink, named pipe,
device or directory where a record belongs is refused without a blocking open
or read: the published record then reads as unusable and silent, and the
attempt record as no prior attempt. A symlink or special entry where the lock
belongs is refused before anything is created or locked, so a dangling link's
target is never created. A successful write replaces whatever entry sits at
the record's name inside the verified directory and never writes through it.
