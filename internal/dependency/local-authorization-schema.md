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
Writers retain verified handles for the authorization directory and every
ancestor, recheck their identity at operation boundaries, and mutate relative
to the opened directory. Replacing an ancestor cannot redirect finalization
or rollback into the project, source, or a concurrent directory. On a reported
failure, rollback replaces only its own pending file identity and matching bytes
with the previous record (or removes its own new record). Concurrent records,
including replacements with identical bytes, are preserved and reported. Project writes use the shared checked journal.
If the project operation completed but authorization finalization failed, the
error explicitly reports possible project changes; it never claims unchanged
state. Restoring an old record restores only preexisting directory access.
Explicit reinstall repairs malformed records and recovers an interrupted
project journal before deriving state or taking an unchanged shortcut; readers
never rewrite records or recover journals.
Temporary snapshot and authorization staging files are removed on success/failure.
The `.lock` file is persistent coordination state, contains no authority, and is
never unlinked during a writer operation.

Before promotion, a closed staging pathname must still name the writer’s retained
file identity, intended bytes and 0600 permissions. A replacement or in-place
mutation is refused and kept at its current path, including when destination or
ancestor validation refuses first. Cleanup removes only unchanged owned closed
staging files through the retained directory handle; activation
failures retain the completed-project diagnostic and original cause.

Readers enforce the same canonical outside-plugin boundary as writers, including
when a completed matching record was copied into the plugin. Before an implicit
dependency write recovers a pending journal, it checks both live local rows and
checked journal before-images. A journal-free GitHub replacement still removes
an unauthorized or unavailable local row without reading its source. Explicit
PATH installation remains the consented journal-repair route.

Removal locks and rechecks even a missing record when its authorization parent
exists. If the parent is absent, offline removal creates no store. After project
work, removal checks that absence again. A parent created concurrently produces
an explicit completed-project conflict; the concurrent grant is preserved and
revocation is not reported as successful. A later explicit installation after
completed removal can legitimately create fresh authority.
