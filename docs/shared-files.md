# Shared instruction files

Claude Code and Codex rules are managed blocks inside ordinary project Markdown. ACR owns only complete `acr:begin`/`acr:end` spans recorded in `.agents/registry.lock`; every byte outside those spans remains operator-owned. See the [preservation contract](preservation.md) for marker parsing, ownership proof, and conflict behavior.

## Instruction roots and hosts

Claude Code treats every existing `.claude/CLAUDE.md` and `CLAUDE.md` as an instruction root. Codex treats every existing `AGENTS.md` and `AGENTS.override.md` as a root. When an adapter has none of its candidate files, ACR uses the bare `CLAUDE.md` or `AGENTS.md` filename.

ACR follows Markdown imports before choosing a host. Selected roots in one connected include graph share their deepest reachable host. Roots without a shared host receive separate blocks. This is why the same package appears once in the first example and twice in the second.

A block ID is:

```text
hex(sha256("acr-block-v1\x00" + source + "\x00" + artifactID + "\x00" + adapterID))
```

The adapter ID is the adapter that owns the chosen host. Path and package version are absent, so an update replaces the recorded block rather than appending another one.

## Example A: CLAUDE.md imports AGENTS.md

Both adapters are selected. The project starts with these files:

`CLAUDE.md`:

```text
# Claude instructions

@AGENTS.md
```

`AGENTS.md`:

```text
# Agent instructions

Existing agent content.
```

The first `acr realize` appends one block to `AGENTS.md`. The roots share that host through the import, and Codex owns the host, so the rule body is not duplicated. The author then adds custom Claude text on both sides of the import and custom agent text on both sides of the block. Content below the managed block can only be added after this first realization creates the block and gives the author a position to write below. A second `acr realize` preserves every custom byte and anchors the new shared-file hash.

`CLAUDE.md` after re-realization:

```text
# Claude instructions

Custom Claude content above the import.
@AGENTS.md
Custom Claude content below the import.
```

`AGENTS.md` after re-realization:

```text
# Agent instructions

Existing agent content.
Custom Agents content added above the block.
<!-- acr:begin id=982f165951b7b23e13a735a402cbcf9f0a71b3760b1c7cf8d0f3e974eb81ee2c source=github:example/shared-files-import artifact=package-rules-9b5bf84aac70cd3deac4a3d654a7d3e0ce88ee05cd398d4e59ad0a1f4cb3f69a adapter=codex prefix=none -->
## ACR package: github:example/shared-files-import

### Rule: team-guidance

# Team guidance

Keep the custom instructions around this rule.
<!-- acr:end id=982f165951b7b23e13a735a402cbcf9f0a71b3760b1c7cf8d0f3e974eb81ee2c -->
Custom Agents content added below the block.
```

A third `acr realize` has an empty plan. Removing `github:example/shared-files-import` produces this byte-exact compiler candidate, including the missing final newline in both files:

`CLAUDE.md`:

```text
# Claude instructions

Custom Claude content above the import.
@AGENTS.md
Custom Claude content below the import.
```

`AGENTS.md`:

```text
# Agent instructions

Existing agent content.
Custom Agents content added above the block.
Custom Agents content added below the block.
```

The current CLI refuses to apply that final shared-Markdown ownership transition before writing; [issue #55](https://github.com/jbaruch/agentic-context-registry/issues/55) tracks moving the compiler candidate from shared to unmanaged ownership. The golden binds the preservation result without claiming that `acr uninstall` already applies it.

## Example B: separate CLAUDE.md and AGENTS.md roots

Both adapters are selected, but `CLAUDE.md` does not import `AGENTS.md`. Each root is its own host. The project starts with these files:

`CLAUDE.md`:

```text
# Claude instructions

Existing Claude content.
```

`AGENTS.md`:

```text
# Agent instructions

Existing agent content.
```

After the first realization, the author adds custom text above and below each block and realizes again. Content below the block can only be added after that first realization creates the block and gives the author a position to write below.

`CLAUDE.md` after re-realization:

```text
# Claude instructions

Existing Claude content.
Custom Claude content added above the block.
<!-- acr:begin id=626ede0e75c0c25d79e12975d1758a4ecec69331f8ab219c76e01b342b5eb88b source=github:example/shared-files-separate artifact=package-rules-4281a3634ff72510d7e14a0117f3a38e04da52b13f47cf6c4baf253af6308062 adapter=claude-code prefix=none -->
## ACR package: github:example/shared-files-separate

### Rule: team-guidance

# Team guidance

Keep the custom instructions around this rule.
<!-- acr:end id=626ede0e75c0c25d79e12975d1758a4ecec69331f8ab219c76e01b342b5eb88b -->
Custom Claude content added below the block.
```

`AGENTS.md` after re-realization:

```text
# Agent instructions

Existing agent content.
Custom Agents content added above the block.
<!-- acr:begin id=615cfda9aedca455ef9f80e9bc095efe3e6f899ba23b1ffec8b867d1e4b0cdcf source=github:example/shared-files-separate artifact=package-rules-4281a3634ff72510d7e14a0117f3a38e04da52b13f47cf6c4baf253af6308062 adapter=codex prefix=none -->
## ACR package: github:example/shared-files-separate

### Rule: team-guidance

# Team guidance

Keep the custom instructions around this rule.
<!-- acr:end id=615cfda9aedca455ef9f80e9bc095efe3e6f899ba23b1ffec8b867d1e4b0cdcf -->
Custom Agents content added below the block.
```

The IDs differ only through the adapter-ID input. A third realization is empty. The final-removal candidates keep all four custom spans and omit both managed blocks:

`CLAUDE.md`:

```text
# Claude instructions

Existing Claude content.
Custom Claude content added above the block.
Custom Claude content added below the block.
```

`AGENTS.md`:

```text
# Agent instructions

Existing agent content.
Custom Agents content added above the block.
Custom Agents content added below the block.
```

Applying those candidates is subject to [issue #55](https://github.com/jbaruch/agentic-context-registry/issues/55). Until it lands, the planner refuses and leaves both files unchanged.

## Golden source

The two examples are checked in as UTF-8 text under `internal/adaptertest/testdata/shared-files-import` and `internal/adaptertest/testdata/shared-files-separate`. `TestSharedFilesImportGolden` and `TestSharedFilesSeparateGolden` run both adapters together, compare the first realization, re-realization, idempotent plan, and byte-level removal candidates, and preserve the final-newline state. The goldens were generated once with `go test ./internal/adaptertest -run 'TestSharedFiles(Import|Separate)Golden' -update`; ordinary test runs only compare them.
