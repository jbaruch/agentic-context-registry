# Local authorization state (schema 1)

Owner: `internal/dependency`. Explicit path installs are the only grant writer.
Local uninstall and replacement by a GitHub request retire grants. Implicit
install, freshness, realization, check, list and outdated are readers; none
migrates, repairs or adopts a missing, invalid, or future-schema record.

Location: `${ACR_STATE_HOME:-<user-cache>/acr}/local/<project-key>/<source-key>.json`.
The base must resolve outside the project and the local plugin. Directories
created here use 0700, records and advisory lock files use 0600. Keys are SHA-256;
project identity uses `freshness.ProjectIdentity`, source key hashes the source.

| Field | Contract |
| --- | --- |
| `schemaVersion` | Integer 1; every other value refuses authorization. |
| `project` | `sha256:` canonical project identity. |
| `source` | Exact `github:owner/package` manifest identity. |
| `path` | Exact normalized declaration path, relative to the selected project or absolute. |
| `sourceRoot` | `sha256:` hash of `acr-local-source-v1` + NUL + canonical source directory. |
| `pending` | Optional boolean; true grants no access. Missing means false. |

There is no timestamp, version or content hash in this record. The grant names
a directory, so later edits can be refreshed by an authorized bare install.
Locked materialization separately requires matching manifest identity, version
and inventory hash. Canonical directory and record bindings are rechecked each run.
Reusing the same project/source locations retains the grant.

Writers take a nonblocking advisory lock at `<record>.lock`. They atomically
replace the record with an inactive pending record, run the checked project
operation, then activate or delete the record. A crash leaves the record inactive.
On a reported failure, rollback replaces only matching pending bytes with the
previous record (or removes its own new record). Concurrent authorization bytes
are preserved and reported. Project writes use the shared checked journal.
If the project operation completed but authorization finalization failed, the
error explicitly reports possible project changes; it never claims unchanged
state. Restoring an old record restores only preexisting directory access.
Explicit reinstall repairs malformed records; readers never rewrite them.
Temporary snapshot and authorization staging files are removed on success/failure.
The `.lock` file is persistent coordination state, contains no authority, and is
never unlinked during a writer operation.
