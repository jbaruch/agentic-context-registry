# Transactional realization and ownership

ACR separates adapter rendering from filesystem authority. An adapter inspects native agent configuration and emits a complete set of rendered target intents. The realization engine validates those intents against the project and the previous ownership ledger, produces a deterministic plan, and applies the plan as one rollback-capable transaction.

Preservation-safe Markdown and structured-configuration compilation is implemented by `internal/preserve` (issue #6). Native destination choices and agent-specific rendering remain tracked in issue #12. Adapters cannot bypass the generic transaction and ownership checks described here. See [`docs/preservation.md`](preservation.md) for marker, graph, JSON, TOML, classification, and transition details.

## Adapter boundary

`internal/adapter` (issue #10) is the versioned boundary adapters render through; it defines the `Adapter` interface, the data-only `Output` kinds, and `compileOutputs`, the sole trusted bridge that turns adapter output into the `Intent` values this document describes. Adapters never construct `Intent` directly and never set its merge-binding fields (`ObservedHash`, `ManagedIntact`, `PreservedContent`) themselves; `compileOutputs` derives them from a registered `SharedCompiler`'s proof, and both kinds needing one fail closed without it. See [`docs/adapters.md`](adapters.md) for the full contract, the capability preflight (`unsupported_adapter_capability`), and the golden-fixture harness.

The planner's own `preserves()` check is vacuously true for an empty `PreservedContent`; independently of the adapter boundary, a shared `ensure`/`promote` whose observed file is non-empty and carries no non-empty preserved fragment is a conflict here too.

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

## Modes and transaction boundary

- Dry-run returns the complete plan without writing files or the ledger.
- Check returns a changes result for any non-empty conflict-free plan and a conflict result for unsafe state.
- Apply preflights every target hash, writes through same-directory temporary files, and invokes a transactional ledger finalizer only after all filesystem operations succeed.

If a file write or ledger finalizer fails, every applied file and Git-exclusion change is restored from its preflight snapshot. A plan also fails if a target changes between planning and application. Running the same successful realization again produces an empty plan.

## Git behavior

In standard and linked Git worktrees, ACR asks Git for the authoritative `info/exclude` path and maintains one marked block there. Only generated-only, untracked targets appear in that block. Existing tracked files remain tracked; ACR never runs `git add`, `git rm`, or otherwise changes the index. Promotion removes the target from the local exclusion in the same transaction that updates its ownership ledger. Bytes outside the marked block are preserved, including a pre-existing missing trailing newline.

Shared targets are never excluded and should be committed as project source. Non-Git projects use the same ownership and transaction rules without Git-specific operations.

## Safe removal

An omitted or explicitly removed generated-only target is deleted only when its current hash and mode match the ledger and Git does not track it. A tracked generated-only target is retained while its ledger ownership is dropped. Changed generated output normally conflicts; a compiler-rendered removal may retain newly added unmanaged bytes when it is bound to the exact observed hash, confirms intact managed regions, and provides a nonempty preservation proof. The same transaction removes its local Git exclusion.

A shared target can be changed only through a compiler-rendered intent bound to the exact observed hash with intact recorded entries and preserved unmanaged content. Removing its final managed entry writes back the unmanaged content and drops the target from the ledger. Omitting a shared target never authorizes whole-file deletion.
