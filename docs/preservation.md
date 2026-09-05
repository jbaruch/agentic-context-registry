# Preservation-safe shared compilation

`internal/preserve` is the production `adapter.SharedCompiler`. It turns data-only Markdown and structured-config contributions into candidate files and preservation proofs. Adapters never provide a host document, observed hash, ownership classification, or preservation fragment.

## Classification

Classification uses the observed bytes and the ownership ledger, never a filename:

- A missing target containing only desired managed regions is `generated-only`.
- A ledger-proven generated target remains `generated-only` while its managed regions are intact and exhaustive.
- Intact generated output that gains an unowned byte or structural entry is promoted to `shared`.
- Existing nonempty content without a ledger is `shared` immediately.
- Existing zero-byte targets fail closed; they cannot produce a non-vacuous preservation proof.
- Shared ownership is sticky. Explicit demotion succeeds only when no unmanaged bytes or entries remain.

Force mode does not change these rules. It cannot adopt unknown marker text, absorb an existing config entry, ignore a damaged managed hash, or replace unmanaged content.

## Markdown blocks

Managed blocks use complete ASCII HTML-comment lines at column zero:

```text
<!-- acr:begin id=<64-lower-hex> source=github:<owner>/<repo> artifact=<kebab-id> adapter=<kebab-id> prefix=<none|lf|crlf> -->
<adapter-rendered body>
<!-- acr:end id=<same-64-lower-hex> -->
```

The ID is `hex(sha256("acr-block-v1\x00" + source + "\x00" + artifactID + "\x00" + adapterID))`. Target paths and version numbers are excluded, so an upgrade replaces the same block in place. `compileOutputs` stamps the registered adapter ID into the compiler request; marker attribution never trusts adapter-supplied identity.

The scanner records byte offsets and splices only ledger-owned spans. Malformed, unmatched, nested, duplicate, attribution-mismatched, or unowned ACR-looking markers conflict. Existing marker line endings remain unchanged. New blocks use the host's first line ending, defaulting to LF.

`prefix` records a separator inserted before an opening marker. The separator belongs to the managed span. Removing a block from a host that originally lacked a final newline therefore restores the original bytes without leaving an extra newline. BOMs, CRLF, mixed line endings, missing final newlines, and every other unmanaged span remain byte-identical.

## Include graphs

`DiscoverIncludeGraph` walks regular project files without following symlinks. Every regular `CLAUDE.md` and `AGENTS.md` is a root. It recognizes `@<normalized-relative-POSIX-path>` as the first non-whitespace token outside fenced code and ACR blocks, then follows included Markdown recursively.

Edges record source paths and lines. Diagnostics are sorted and carry resolved chains. Invalid or unresolved targets, duplicate direct or transitive paths, and cycles block selected roots in the affected connected component. The same findings remain warnings for unrelated components. `Reachable` supports transitive include reuse; `DeepestSharedHost` chooses an existing common host deterministically.

`DiscoverSelectedIncludeGraph` is the realization variant: an include naming one of the instruction hosts that run writes resolves even when the file is absent, because the run creates it. Deselecting every Markdown adapter removes an ACR-created host while a user's import of it survives, and reselecting must discover the host before it can regenerate it. `DiscoverIncludeGraphSnapshot` keeps the plain reading, where a missing target is unresolved like any other.

## JSON and TOML

JSON compilation validates with `encoding/json` and uses a byte-offset scanner. TOML compilation uses the pinned `github.com/pelletier/go-toml/v2/unstable` parser for decoded keys and source ranges. Neither format is unmarshaled and remarshaled as a document.

Existing key order, whitespace, comments, and unowned raw values remain in place. Owned values update at their current offsets. New entries are sorted by structural identity. Removal deletes only the recorded member or array element and its required delimiter. Duplicate decoded JSON keys, duplicate fully qualified TOML keys or tables, repeated desired locations, identical managed elements in one container, and multiple matches for one managed hash all conflict.

A structured managed hash binds a domain separator, format, decoded container path, entry kind, field key where applicable, and exact raw value. Array elements are found by this hash instead of their position, so reordering unowned siblings does not transfer ownership.

The TOML dependency is pinned to v2.4.3. Weekly Go module Dependabot updates are the renewal mechanism; position and merge contract tests protect upgrades.

## Ownership transitions and removal

Promotion returns shared ownership, a nonempty proof, and `shared_file_requires_commit`. The realization transaction removes the target's local Git exclusion while applying the promoted file and ledger. ACR does not stage the file.

Removing the final package from a shared target writes back only unmanaged content and drops the ledger target. A changed generated-only target can take the same path when the compiler proves the exact observed hash, intact managed regions, and nonempty preserved content. An unchanged untracked generated-only target can still be deleted as wholly owned. A tracked target is retained when ownership is dropped, regardless of its former ownership state.

Dry-run reports `promote`, `demote`, `remove`, and Git-exclusion changes without writing. Apply performs the file, ledger, and exclusion changes in one rollback-capable transaction.

## Tests

Unit tests cover graph diagnostics, marker conflicts, byte-preserving Markdown updates, JSON/TOML structural updates and removals, promotion, sticky sharing, explicit demotion, hostile force, and hash-based array lookup. Text-only goldens live under `internal/preserve/testdata` and run through `adaptertest.RunGolden`; `-update` rewrites only each case's `want/` directory.
