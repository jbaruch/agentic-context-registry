# Transactional realization and ownership

ACR separates adapter rendering from filesystem authority. An adapter inspects native agent configuration and emits a complete set of rendered target intents. The realization engine validates those intents against the project and the previous ownership ledger, produces a deterministic plan, and applies the plan as one rollback-capable transaction.

Preservation-safe Markdown and structured-configuration compilation is implemented by `internal/preserve` (issue #6). Native destination choices and agent-specific rendering remain tracked in issue #12. Adapters cannot bypass the generic transaction and ownership checks described here. See [`docs/preservation.md`](preservation.md) for marker, graph, JSON, TOML, classification, and transition details.

## Adapter boundary

`internal/adapter` (issue #10) is the versioned boundary adapters render through; it defines the `Adapter` interface, the data-only `Output` kinds, and `compileOutputs`, the sole trusted bridge that turns adapter output into the `Intent` values this document describes. Adapters never construct `Intent` directly and never set its merge-binding fields (`ObservedHash`, `ManagedIntact`, `PreservedContent`) themselves; `compileOutputs` derives them from a registered `SharedCompiler`'s proof, and both kinds needing one fail closed without it. See [`docs/adapters.md`](adapters.md) for the full contract, the capability preflight (`unsupported_adapter_capability`), and the golden-fixture harness.

The planner's own `preserves()` check is vacuously true for an empty `PreservedContent`; independently of the adapter boundary, a shared `ensure`/`promote` whose observed file is non-empty conflicts unless it carries at least one non-empty preserved fragment and every non-empty fragment occurs byte-for-byte in the hash-bound observed file.

## Ownership states

- `generated-only`: every meaningful byte or structured entry in the target is owned by ACR. An untracked target is locally Git-excluded.
- `shared`: ACR owns only the ledger entries; unmanaged content remains authoritative project source and is never locally excluded.
- `unmanaged`: ACR has no deletion or overwrite authority. Unmanaged targets are not written to the ledger.

Generated-only output that gains unmanaged content must move through an explicit `promote` plan. The adapter binds its merge to the exact observed file hash, confirms that managed content is intact, and supplies unmanaged byte fragments that the rendered result must preserve. Shared ownership is sticky: a plan cannot move back to generated-only without `ExplicitDemotion`.

## Plan operations

Plans use the following stable operation names:

| Operation | Meaning |
| --- | --- |
| `create` | Create a missing generated-only target |
| `update` | Replace unchanged, wholly owned output |
| `merge` | Change only adapter-rendered ownership inside a shared target |
| `preserve` | Explicitly retain an unchanged target |
| `promote` | Move generated-only output to shared ownership |
| `demote` | Explicitly move shared output to generated-only ownership |
| `conflict` | Refuse a write or removal because authority or preconditions are insufficient |
| `remove` | Delete proven wholly owned output, or remove final managed entries while retaining a shared file |

File bodies are intentionally omitted from JSON plan output. Paths, before/after hashes, ownership transitions, modes, reasons, and Git-exclusion changes remain reviewable.

## Ownership ledger

`.agents/registry.lock` stores a `realization` object alongside immutable dependency resolutions:

```yaml
realization:
  schemaVersion: 1
  targets:
    - path: .claude/rules/team-guidance.md
      mode: 420
      ownership: generated-only
      outputHash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      excluded: true
      entries:
        - source: github:example/team-context
          artifactId: team-guidance
          artifactKind: file
          sourcePath: rules/team-guidance.md
          adapter: claude-code
          adapterVersion: 1.0.0
          managedHash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Each target records the complete output hash and ownership composition. Each owned entry records its dependency source, logical artifact and source path, ownership granularity, adapter identity/version, and managed-content hash. Paths under `agents.yaml`, `.agents`, and `.git` are reserved and can never be authorized as adapter targets. The machine-readable shape is part of [`schemas/registry-lock.schema.json`](../schemas/registry-lock.schema.json).

A shared-surface target carries `owner: coordinator` and lives only under `.agents/skills/acr__…`. The discriminator and the path prove the same thing: a coordinator-owned target elsewhere, or a shared-surface path with no owner, is refused rather than resolved.

The ledger is graded the way project state is. Readers accept `schemaVersion` 1 and 2; 2 is written only when the ledger owns at least one coordinator target, so a project that never gains a shared surface stays readable by an ACR that predates it, and one that does gains a loud `unsupported realization schemaVersion 2` there instead of a silently ignored field.

## Checkout-local evidence

`.agents/.acr-applied.json` records the generated-only targets this checkout last successfully applied. Git can replace the committed ledger during a pull without changing ignored native files; the receipt retains the hashes needed to distinguish those old outputs from user edits. Each linked worktree has its own receipt. Non-Git projects use the same path.

`internal/realizeapp` owns the schema and is its only writer. Writers emit `schema_version: 2`; readers accept only version 2. Version 1 receipts are unusable evidence and fall back to committed comparison, without upgrading or trusting their historical hashes. The JSON object requires `schema_version`, `ledger`, `checkout` and `files`. The ordinary nested ledger has its own supported `schemaVersion` (1 or 2), with the complete target shape above: paths, modes, output hashes, ownership entries and exclusion flags. Every target must be `generated-only`; a single `file` entry's `managedHash` must equal its target's `outputHash`.

`checkout` records the checkout root directory's `device` and `inode`. Each item in `files` requires `path`, `device`, `inode`, `size`, `mtime_seconds`, `mtime_nanos`, `ctime_seconds` and `ctime_nanos`, obtained from `os.Stat`/`syscall` metadata on Darwin and Linux after native outputs are applied. File bindings correspond exactly to the ledger's targets. Readers require the live checkout identity and each target's complete metadata to match before accepting its receipt hash. Missing, changed or unavailable metadata grants no receipt permission for that target; committed comparison still applies. An older CLI generating B and a user restoring A's bytes, Git switching a tracked output away from A followed by restoring A, and mixed backup restores invalidate the file binding even when the hash and mtime are restored. A receipt copied from another checkout fails its checkout binding. Platforms without this metadata write no target evidence.

Readers walk the exact JSON schema and validate the ledger and file bindings, rejecting duplicate or case-variant keys, unknown fields, nulls, wrong types, missing required fields, canonical duplicate target paths, invalid hashes, unsupported versions, malformed JSON and trailing values as unusable evidence. Optional ledger targets, owner and excluded fields may be omitted, but never null. No registry-lock schema change is involved.

The committed ledger alone determines ownership, planning paths, adapter coverage, omission, retention and deletion. For a committed generated-only target, unchanged live bytes and mode may match either the committed tuple or an eligible receipt tuple, preferring the committed match. Receipt eligibility requires the same canonical destination, target owner and complete artifact identity set (source, artifactId, artifactKind, sourcePath and adapter). Hashes and adapterVersion are comparison data. A receipt never mixes a hash from one tuple with a mode from another. Receipt-only targets and claims on shared targets are ignored; they authorize no write or deletion. The compiler uses prior entry hashes only when the complete observed file matches eligible evidence, retaining committed ownership and attribution.

### Rolling back Codex and Cursor hook adapters

When rolling back from adapter 1.1.0 to the preceding 1.0.x adapters, keep the current `.agents/registry.lock` and native output in place. Run the older ACR CLI’s `acr realize`, followed by `acr check`, before restoring historical project state. The current lock still records ownership of `.codex/hooks/acr-hook.py` and `.cursor/hooks/acr-hook.py`, allowing the older CLI to remove them and restore its hook definitions. After both commands succeed, restore any required historical `agents.yaml`, lockfile and corresponding tracked shared configuration together, then run the older CLI’s `acr realize` and `acr check` again.

Do not restore the historical lock first. That removes the translator ownership records before cleanup; the local receipt cannot authorize their deletion. The older CLI can then report a current state while leaving orphan translators, and a later upgrade can fail with `realization_conflict`. If this already happened, restore the matching upgraded lock and shared configuration before performing the ordered rollback. Preserve edited files and resolve reported conflicts; do not grant receipt-only files deletion authority.

### Receipt maintenance

The receipt has no authenticated provenance. Someone who deliberately fabricates same-identity evidence containing an edited file's exact hash, mode, live checkout identity and file metadata can cause that edit to be accepted and replaced. Metadata binding prevents accidental stale-evidence replay; it does not authenticate the writer. The receipt is local historical evidence, not a security boundary against its own writer.

A successful full realization maintains the receipt with the applied generated-only ledger. An explicit `--agent` selection replaces only the processed agents' evidence and keeps only eligible omitted agents' actual prior evidence, even if the incoming lock already records newer output. Coordinator targets follow the shared-surface selection boundary. Deleted targets and targets promoted to shared ownership leave the processed receipt. Failed or conflicting runs never seed evidence from observed files. A converged legacy checkout can run the upgraded CLI before pulling a lock bump to establish evidence.

The receipt and `.agents/.gitignore` (containing the anchored rule `/.acr-applied.json`) participate in the same journal as outputs and dependency state. Existing ignore-file content and permissions are preserved; the rule is installed before the receipt. The ignore file converges when a canonical receipt rule occurs after every unescaped negation. A later unrelated negation may cause one appended canonical rule; subsequent converged runs leave auxiliary files untouched. Preparation rechecks eligible target bindings under the apply claim. The transaction verifies every state file against its preparation bytes, permissions and existence before any write, including an intended no-op, and refuses stale state with retry guidance. Parent paths are validated and existing mode-000 state files are refused before opening; deliberately restore readable permissions and retry.

When the native plan has no changes, the receipt is rendered and compared before journaling. Fully converged receipt and ignore files remain untouched, without journal creation or writes. Missing, stale, corrupt or wrong-mode receipts still receive transactional maintenance. Plans with native mutations render the receipt after output renames establish final file metadata; its actual after-hash is durably journaled before the receipt write. Both auxiliary files use regular-file reads bounded at 32 MiB and reject symlinks. Check and dry-run treat an oversized receipt as unusable evidence and fall back to committed comparison; old outputs after a pulled lock bump can still conflict. Apply refuses an oversized receipt before journaling because its before-image exceeds the supported bound: back up/remove the named local receipt and retry `acr realize`, preserving `.agents/registry.lock`. Oversized ignore files and other I/O or unsafe-path failures remain refusals in every mode.

Successful `acr realize`, the realization phase of `acr uninstall`, non-finalizing `acr migrate tessl` coexistence apply, and install-mode freshness maintain the receipt and ignore rule. `acr migrate tessl --finalize` leaves both unchanged; a subsequent realization may refresh them. Check, dry-run and none/outdated freshness do not maintain them. Auxiliary-only maintenance does not report native drift or request agent restarts. Receipt and ignore-file maintenance is excluded from returned realization operations and change counts. Rollback and crash recovery restore these files with the other transaction files. Restoring outputs may change their metadata and invalidate old evidence; recovery never invents a new binding.

When uninstall or migration fails to remove a vendor tree after a successful realization, restoration recovers native outputs and dependency state only. It neither reads nor writes the receipt or `.agents/.gitignore`, so successful forward auxiliary writes remain in place, including files that were initially absent. Concurrent auxiliary edits, even during restoration preparation, remain untouched and do not gate product restoration. This differs from a failure inside the realization transaction, whose journal still rolls back its own auxiliary writes.

The leftover receipt does not expand ownership. Restored outputs are described by the restored committed ledger; receipt comparisons still require matching checkout and file metadata, destination and artifact identity. Recreated or changed outputs invalidate their old bindings, while unchanged surviving outputs may retain eligible evidence. The next check uses the restored product state without auxiliary writes; the next ordinary realize refreshes the receipt and maintains the ignore rule. Genuine generated-file edits remain conflicts. A failure before realization writes auxiliary files never compensates them.

The receipt never enters the committed lock or generated-target inventory. ACR never stages it and refuses an already tracked receipt, including filesystem aliases of case-insensitively matching indexed spellings, with guidance to run `git rm --cached -- .agents/.acr-applied.json`. Index queries and metadata inspection are limited to those spellings. Hardlink-alias detection is best effort within those candidates; unrelated tracked paths are not scanned. Keep `.agents/.gitignore` when later initializing a non-Git project as a repository so ordinary staging still excludes the receipt.

Missing, corrupt or unsupported evidence grants no additional overwrite permission: ACR falls back to the committed-ledger comparison and supplies recovery guidance when it refuses. Missing evidence is normal on first use, and a safe successful realization seeds it. ACR cannot reconstruct evidence lost before an upgrade or after deletion. If a checkout already pulled a bump without usable old evidence, review/back up edits, delete only the listed generated-only files, and run `acr realize` once. Preserve shared files and `.agents/registry.lock`; see [troubleshooting](troubleshooting.md#conflicts-and-ownership).

## Shared skill surface

`.agents/skills` is a surface any generic consumer reads, not an agent tree. A project whose `agents.yaml` sets `sharedSkills: true` gets one coordinator-owned copy of every package skill at `.agents/skills/acr__<workspace>__<package>__<skill>`, with the same content and modes a per-agent skill tree receives, ledger-owned and hash-verified like any other target. Rules and hooks stay out of it: a generic consumer has no hook runtime and no rule-activation vocabulary.

The surface is compiled by the coordinator, not by an adapter, and validated through a predicate separate from the adapter boundary's. `realize.ValidateTargetPath` keeps refusing every `.agents` path, so no package can name `.agents/registry.lock`, `.agents/vendor/**` or `.agents/.acr-transactions/**`; `realize.ValidateSharedSurfacePath` accepts only `.agents/skills/acr__…`, and the engine's ledger, planner, transaction and journal checks accept either.

Ownership follows the same rule `--agent` already follows for per-agent outputs. An invocation covering every configured agent owns the surface and re-derives it; a temporary `--agent` subset omitting one leaves the surface exactly as it is, so no entry is orphaned. `acr uninstall` realizes across the configured/ledger union and therefore removes the uninstalled package's shared entries; clearing `sharedSkills` retires the whole surface through the ordinary removal path.

An existing file or directory occupying an `acr__…` path is a realization conflict: nothing is overwritten. `.agents/skills` being a symbolic link is refused, because ACR writes real files there and never writes through a link it does not own.

## Modes and transaction boundary

- Dry-run returns the complete plan without writing files or the ledger.
- Check returns a changes result for any non-empty conflict-free plan and a conflict result for unsafe state.
- Apply takes an exclusive non-blocking project transaction claim, recovers any prior complete journal before planning, preflights every target hash, and writes through same-directory temporary files.

Before the first target rename, schemaVersion 1 before-images and their hashes, sizes, modes, and planned after-hashes are synced below `.agents/.acr-transactions/.staging-<id>` and atomically renamed to a canonical journal. Native files, Git exclusion changes, `agents.yaml`, `.agents/registry.lock`, `.agents/.acr-applied.json`, and `.agents/.gitignore` are one recovery unit. The journal is removed only after all final state bytes are durable.

Ordinary directories are retained, including empty scaffolding left by file removal or already present in a skill tree. Native validation walks every descendant leaf: unknown files, symlinks, special entries, and inspection errors still fail. A previously ledger-owned whole file may pass validation so the planner can check its recorded hash and mode, including when it was modified. File removals and ledger updates remain in the same transaction and roll back together on failure; existing directories keep their permissions through rollback and recovery.

A retry restores targets that match the after-state, leaves targets already at the before-state alone, and fails with `recovery_conflict` without touching anything when a target matches neither. Unsupported journal versions fail closed. Dry-run and check do not claim or recover; they return `pending_transaction` for a canonical journal and report incomplete `.staging-*` residue without deleting it. Mutating commands remove stale staging under the claim. Filesystems must support advisory `flock`; lock contention is `transaction_busy`, while unsupported lock semantics are `transaction_lock_unavailable` with no fallback. While a regular `tessl.json` is installed, a plan targeting `.tessl/**` or a `tessl__*` native path is refused as `tessl_owned_target` at exit `4` by `acr realize` and `acr check`.

Successful mutating commands retire their transaction claim and remove empty `.acr-transactions` residue, including on converged runs. A pre-existing `.agents` directory and unchanged `registry.lock` remain intact. Failed acquisition may leave an inert `.lock`; a later successful command cleans it safely. Do not delete a claim while another command may be running. Concurrent mutations require updated ACR processes using this retirement protocol; mixing older and newer binaries is unsupported.

## Session-start freshness realization

`freshness: outdated|install|none` in `agents.yaml` controls an ACR-owned session-start hook. When the field is absent, realization deterministically selects `outdated` and an apply persists that value. For `outdated` and `install`, `internal/realizeapp` appends a synthetic ACR package after all locked packages before invoking the unchanged adapter coordinator. `none` appends nothing, allowing the normal ledger-driven removal path to clean up only the previously owned hook.

The hook's `outdated` execution uses a read-only interface to dependency status and writes only its machine-local throttle record outside the project. Its `install` execution calls dependency reconciliation with `dryRun=false` and then this same realization service in apply mode. Reconciliation refreshes only declarations requested as `latest`; explicit tags and commits remain pinned. Both paths consult the dependency rollback-hold seam before accepting a newer release.

Install-mode restart guidance is derived from the returned plan. Any non-`preserve` file operation marks the owning native adapter as affected; shared targets use current or previous ledger entries to identify their adapters. A ledger-only change does not require a restart. Reconcile and realization failures retain their existing transactional guarantees and are converted to session-start notices by the freshness application boundary.

## Git behavior

In standard and linked Git worktrees, ACR asks Git for the authoritative `info/exclude` path and maintains one marked block there. Only generated-only, untracked targets appear in that block. Existing tracked files remain tracked; ACR never runs `git add`, `git rm`, or otherwise changes the index. Promotion removes the target from the local exclusion in the same transaction that updates its ownership ledger. Bytes outside the marked block are preserved, including a pre-existing missing trailing newline.

Shared targets are never excluded and should be committed as project source. Non-Git projects use the same ownership and transaction rules without Git-specific operations.

## Safe removal

An omitted or explicitly removed generated-only target is deleted only when its current hash and mode match the ledger and Git does not track it. Explicit uninstall or retirement of an output surface retains tracked generated-only bytes and releases ledger ownership. The same ownership release applies to unchanged tracked standalone scripts dropped by a still-installed package; modified dropped scripts remain conflicts. When a still-desired dependency drops any other generated-only whole file on an enabled surface, including Cursor rules and native or shared skills, realization instead refuses a tracked file before any ledger write, naming the path and recorded `managedHash` with guidance to review the file, remove it from Git and the working tree, and retry. This preserves ownership evidence until the file is removed. An already-missing indexed file does not block realization. Changed generated output normally conflicts; a compiler-rendered removal may retain newly added unmanaged bytes when it is bound to the exact observed hash, confirms intact managed regions, and provides a nonempty preservation proof. The same transaction removes its local Git exclusion.

A shared target can be changed only through a compiler-rendered intent bound to the exact observed hash with intact recorded entries and preserved unmanaged content. Removing its final managed entry writes back the unmanaged content and drops the target from the ledger. Omitting a shared target never authorizes whole-file deletion.

Native skill-tree validation tolerates a real directory named exactly `__pycache__` when its direct children are all regular files with a case-sensitive `.pyc` suffix, including an empty cache. Empty directory scaffolding and historical managed files retain their existing validation behavior; other unexpected files, symlinks, and special entries remain refused.

Undeclared runtime caches never enter the ownership ledger. Realize, check, dry-run, in-place helper updates, uninstall, and rollback leave those caches untouched. Removal remains ledger-driven: ACR removes only authorized managed files and drops their ownership, leaving unowned caches and their containing directories in place. Declared `.pyc` assets remain ordinary managed files subject to the existing hash, mode, Git tracking, and transaction rules.
