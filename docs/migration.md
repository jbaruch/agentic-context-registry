# Tessl migration

`acr migrate tessl` converts an existing Tessl consumer project to ACR. The default operation establishes coexistence; `--vendor-unmapped` preserves packages that have no GitHub source; and `--finalize` removes positively identified Tessl output only after ACR has converged and the migration is recoverable.

## Command

```text non-executable
acr migrate tessl [--mapping-file PATH] [--map FROM=github:owner/repository[@REQUESTED]] [--vendor-unmapped] [--finalize] [--dry-run] [--json] [--project PATH]
```

Follow the [end-to-end migration guide](migration-guide.md) for the staged producer and consumer journey.

`--dry-run` performs resolution, materialization, semantic comparison, and planning but writes nothing. Without it, ACR writes only ACR-owned files and state in coexistence mode. `tessl.json`, `.tessl/**`, `tessl__*` natives, Tessl-managed host spans and objects, and the Tessl `.gitignore` block remain byte-identical. ACR never hosts a managed block inside a Tessl-owned path.

`--finalize` is a separate transaction and never applies pending coexistence changes. Every refusal the planner reaches — including the pending-coexistence and Git-tracking gates, a layout the removal cannot address, and a run whose transaction rolled back — reports `finalizationReady: false`, names itself in `blockers[]` with a remedy, and carries the invocation's own `dryRun` mode. A rolled-back run reports no removals and no re-anchors: nothing it planned survived, and the text says `refused`, never `applied`.

Recovery either finishes or it does not. A concurrent write to a file the transaction had already changed leaves the target matching neither the planned state nor the recorded before-image; recovery refuses to overwrite it and preserves its journal, and the refusal says so and names the journal to reconcile against instead of certifying a complete restore. It exits `4` with `finalization_blocked` while any effective diff, lossy mapping, unmapped package, ambiguous artifact, uncovered agent, unretirable shared skill link, unprovable Tessl MCP entry, untracked `tessl.json`, or untracked vendor file remains. A refusal the planner reaches names itself in `blockers[]`, with the path and the remedy that clears it, and its report is returned alongside the error in both text and JSON. An unmapped package is rejected before a report is constructed: that refusal exits `4` with `finalization_blocked` and an actionable error naming the package and the `--map`/`--mapping-file` remedy, and carries no migration `result` and no `blockers[]` in either text or JSON. Supply the mapping and re-run to reach the planned refusals. A successful run removes Tessl-owned files and byte spans, preserves unrelated siblings, re-anchors shared ACR ledger targets, and reports every removal and retained item. An ACR block reachable only through a Tessl include chain is unreachable: host selection and plan validation forbid ACR targets inside Tessl-owned paths. `tessl.json` is removed last; `.agents/registry.lock` is written last.

If the migration realization plan targets `.tessl/**` or a `tessl__*` native path, `acr migrate tessl` exits `4` with `tessl_owned_target` before applying the plan. The same refusal protects `acr realize` and `acr check` while a regular `tessl.json` remains installed.

A converged second apply reuses the lock and makes zero mutable-resolution calls (`LatestRelease` and `ResolveCommit`) and no project writes. It still downloads the immutable package archive to materialize and compare ACR artifacts on every run; eliminating that retrieval requires the verified package-content cache tracked in [issue #50](https://github.com/jbaruch/agentic-context-registry/issues/50).

The pure inventory service opens the project through `adapter.NewRootSnapshot`, whose API is `ReadFile`/`ReadDir` only. `internal/migrate` imports neither `os` nor `internal/realize`; network and writes are confined to `internal/migrateapp` and the realization engine.

## Package mappings

Mappings use strict precedence: a repeatable `--map` wins over `--mapping-file`, which wins over an explicit repository in the Tessl plugin manifest. ACR never derives a repository from the Tessl package name. Within one tier, identical duplicates collapse and contradictory duplicates fail before any write.

```yaml
schemaVersion: 1
packages:
  - from: tessl-labs/good-oss-citizen
    source: github:tesslio/good-oss-citizen
    requested: v1.1.12
```

When `requested` is omitted, Tessl's `latest` remains `latest`. A pinned Tessl package version such as `1.1.12` must match exactly one stable GitHub release tag named either `1.1.12` or `v1.1.12`. An explicit `@REQUESTED` value is used as written. The mapped repository must already publish a valid `agent-plugin.yaml`; otherwise migration returns `source_not_a_package` and points to producer migration #11 and publishing #9.

An unmapped package blocks the entire run unless `--vendor-unmapped` is present. Add an explicit mapping when a publishable ACR package exists; otherwise vendor the installed bytes.

## Vendored packages

`--vendor-unmapped` copies each still-unmapped `.tessl/plugins/<workspace>/<package>` tree to `.agents/vendor/<workspace>/<package>`. An explicit `--map` for the same package wins and prevents the vendor copy. The copy accepts regular files only, rejects symlinks and unsafe paths with `vendor_escape`, normalizes directories to `0755` and files to `0644` or `0755`, and promotes a fully staged tree with one rename.

The declaration is `source: vendor:<workspace>/<package>` with `requested: vendored`. Its lock has `kind: vendor`, the Tessl version in `packageVersion`, and a domain-separated SHA-256 over every regular file, including undeclared README, test, and metadata files. It has no commit, tag, or release ID. Realization re-hashes the local tree, synthesizes its artifact manifest in memory, makes no network request, and returns a no-op cleanup so the persistent tree cannot be deleted accidentally.

A later `--map <workspace>/<package>=github:owner/repository[@REQUESTED]` may supersede the vendor source. ACR compares the normalized `(kind, id, activation, event, digest)` sets. It rewrites source ownership and removes the verified vendor tree only when those sets are equal; otherwise `effective_mismatch` exits `4` without writes.

Vendored dependencies remain visible in `acr list` and in a non-actionable section of `acr outdated`. `acr install`, targeted `acr update`, and `acr resume` refuse `vendor:` sources and direct the user back to migration. When a real upstream differs and `--map` cannot supersede the vendored package, `acr uninstall vendor:<workspace>/<package>` is the forward path: it prunes the dependency, realizes every covered agent, then removes the ACR-owned vendor tree through the recovery journal.

## Finalization and recovery

Finalization removes only positive Tessl evidence: `.tessl/**`, `tessl.json`, declared `tessl__*` native artifacts, marked Markdown spans, exact Tessl structured hook elements, the canonical Tessl MCP server entry, and the Tessl-owned `.gitignore` block. Unrelated content is retained. `.agents/` is never removed because it contains the ACR lock and vendor tree.

### Shared skill surface

`.agents/skills` is not an agent tree. It is a shared surface any generic consumer reads, which Tessl writes in addition to the per-agent trees, and its coverage is computed per entry rather than declared.

ACR owns the surface for a project whose `agents.yaml` sets `sharedSkills: true`, materializing `.agents/skills/acr__<workspace>__<package>__<skill>` as real files with the same content and modes a per-agent skill tree receives. Migration sets the field when Tessl already wrote the surface; `acr init` never does. Entries are real files, not symlinks: a symlink carries no `outputHash` in the ledger, and one pointing into a per-agent tree would break when that agent is de-selected. Rules and hooks stay out of the surface, since a generic consumer has no hook runtime and no rule-activation vocabulary.

Finalization retires a `.agents/skills/tessl__<id>` entry only when **all** hold: it is a symlink; its own target names `.tessl/plugins/<identity>/skills/…` of an installed package without following the link and without erasing an uninspected `name/..` component; that skill is `migratable` with an empty `lossy` list; and the realization ledger already owns the matching `.agents/skills/acr__…` entry. Every other entry is classified explicitly:

| Entry | Outcome |
| --- | --- |
| not prefixed `tessl__` | untouched; only its link target is read, so a retained dependency can be checked |
| a real directory or file | retained, `non-symlink-shared-entry` |
| a symlink leaving the project | retained, `skill-tree-escape`; the target is never followed |
| a symlink outside `.tessl/**` | retained, `foreign-shared-link`; finalization removes its target only if the plan below names it |
| a `tessl__` symlink whose stored `..` would erase an uninspected component | retained, `unproven-shared-link-target`; finalization then checks survival without following it |
| a per-agent `tessl__` link whose stored destination is not this package's plugin tree | retained, `unproven-native-link-target`; the user's own discovery entry is never a removal |
| a symlink into `.tessl/**` naming no migratable declared skill | **blocker** `shared-skill-orphan` |
| a removable link with no ACR equivalent yet | **blocker** `shared-skill-replacement-missing` |
| any retained entry whose route, or an ancestor on it, this run removes or prunes | **blocker** `shared-skill-dangling-dependency` |
| any retained entry whose entrypoint or bundled dependency this run removes | **blocker** `shared-skill-dangling-dependency` |
| any retained entry reaching its target through a link ACR does not own | **blocker** `shared-skill-unproven-dependency` |
| any retained entry whose inspection cannot be completed within the budgets | **blocker** `shared-skill-unproven-dependency` |
| a removable link that changed since it was inventoried | **blocker** `shared-skill-ownership-changed` |

A retained entry is only safe while its route and the skill it opens survive, and finalization removes files well outside `.tessl` — every per-agent `tessl__` native it positively owns is a deletion too. Every entry ACR keeps, including a user's own alias and a real directory the user put on the surface, is therefore compared against the removal plan that was actually built, before any mutation is staged. The comparison covers the complete effect of that plan: the planned deletions plus every `.tessl` directory the pruning pass empties, an initially empty directory and a recursively emptied ancestor included. A route that traverses a directory today and loses it to pruning is a dangling dependency, not a survivor. Its target is read, never followed for ownership, and an absolute pathname is inspected component by component rather than assumed to be an escape: an absolute target can name a file inside this very project. Reading a link grants no deletion ownership.

The target path is walked one stored component at a time, because comparing a cleaned target misses the ways a link breaks. A Tessl skill tree is itself a symlink and the plan deletes that one link rather than each path beneath it, so an alias into a nested directory loses an **ancestor** rather than its own target. A component inside the project that is itself a link ACR does not own leads somewhere the walk cannot establish; following it is the traversal that grants no ownership and can leave the project, so the dependency is unproven and the run refuses rather than guessing. The walk inspects components in filesystem order before discarding any name that can carry a dependency: lexical cleaning of `shortcut/..` drops the unowned link the kernel actually follows, so that link is unproven rather than equivalent to deleting the name. The same `name/..` shape also refuses `tessl__` retirement, on the shared surface and on a per-agent surface alike: the cleaned destination is not equivalent ownership. Leading `../` out of the directory that holds the entry, and `.` or `..` through real directories, stay ordinary path motion.

Once the route leaves the project, an external link is not accepted for the endpoint it evaluates to. Its own stored target is expanded into the walk and every component of that target is inspected in turn, so a sibling alias that identifies the project through `.tessl` state this run removes is a dangling dependency rather than a proven identity. Expansion is bounded and cycle-safe: a route that expands past the link budget, or that cycles, is an unproven-dependency refusal. A genuine external route through real directories survives, including ordinary `.` and `..` motion and normal `/tmp` placement.

Membership in the removal is decided on the identity the filesystem reports, not on the spelling alone. A route that reaches a removed directory under a differently cased name on a case-insensitive filesystem is recognized as reaching it; no name is lowercased or case-folded, so two distinct objects on a case-sensitive filesystem stay distinct. A directory cannot be hard-linked, so identity settles it for one; a non-directory can carry several entries on one inode, and unlinking the planned entry then does not unlink the other, so identity alone there is an unproven-dependency refusal rather than either proof or safety. Failed inspection or identity comparison is an actionable refusal, never proof of safety.

The retained skill's contents are inspected as well as its top-level entry, because preserving link bytes preserves nothing an agent can open. The entrypoint and the bundled entries beneath the proved endpoint are enumerated through real directories inside that subtree, and each nested link's own route is proved with the same walk rather than crawled through. Bundled code is never executed and no dependency is inferred from prose. Enumeration is bounded by finite entry and depth budgets; exhausting one, or failing to read an entry's metadata, is an unproven-dependency refusal. A normal finite external skill with a regular entrypoint, regular bundled files and real directories succeeds unchanged.

A demonstrated missing component, or a non-directory with a remaining suffix, ends the walk as already broken: a route that no longer resolved before this run is not this run's to repair or to refuse. An uninspectable component is not that. Raw user link targets and outside content remain unchanged. Comparisons are on path components, so a neighbour whose name merely starts with a removed path's name is never mistaken for one.

Per-agent retirement is proved the same way. `.claude/skills`, `.codex/skills` and `.cursor/skills` each keep their canonical `tessl__<id>` retirement, and a matching basename authorizes nothing on its own. Planning reads the destination the link actually stores — the same bytes it places in the transaction's before-image — and retires the link only when that destination is the installed package's own `.tessl/plugins/<identity>/` tree, which the same run removes in full. A repointed, absolute, or `name/..` destination is retained with `unproven-native-link-target`, never rewritten and never reported as a removal, and its own survival is then proved like any other retained entry. A link repointed after it was inventoried is caught by that same read.

A link naming `.tessl` state this run deletes is never retained: the target goes away and the link dangles, which is the stale-reference class finalization exists to prevent.

`.agents/skills` being a symbolic link fails the inventory outright, naming the remedy: ACR writes real files there and never writes through a link it does not own.

`.openhands/skills` has no ACR adapter and no mapped equivalent, so it blocks with a named remedy the way `gemini`, `github` and `vscode` do rather than losing its skill links silently. `.github/mcp.json` is GitHub's evidence too: an MCP-only GitHub consumer keeps the uncovered-agent guard instead of finalizing with its `tessl mcp start` integration left in place.

### Instruction hosts

A Tessl-managed heading owns everything to the next same-or-higher heading or to end of file. A consumer that installed Tessl before ACR therefore ends up with ACR's own block appended inside that span, where it would read as extra content and make the whole host ambiguous. The span ends at ACR's proven ownership instead, so finalization retires only Tessl's heading and its includes and leaves ACR's block and all user prose byte-identical. The boundary comes from the same parser the preservation compiler uses: a marker-shaped user line, an unclosed marker or plain prose inside the span establishes nothing, and the host still refuses.

### MCP retirement

ACR authors no MCP server entry. Finalization retires the one Tessl integration it can positively identify, and nothing else.

Automatic retirement fires only when **all** hold: the map key is exactly `tessl`; the entry sits in the recognised server map of a **supported** agent's config; `type` is `"stdio"`; `command` is the bare name `tessl` with no path separator; `args` deep-equals `["mcp","start"]`; and the entry carries no other key. That is the object real Tessl 0.105.0 writes, verified against Claude Code, Codex and Cursor output, so an ordinary consumer cuts over with no prompt and no flag.

The mutation list is three files, named rather than derived from the detection list: `.mcp.json`, `.cursor/mcp.json`, and `.codex/config.toml`. `.vscode/mcp.json`, `.github/mcp.json` and `.gemini/settings.json` belong to agents ACR neither supports nor replaces and are never edited. Presence alone classifies nothing: a config holding no Tessl entry is ordinary user configuration.

A name alone never authorizes deletion, and a shape alone never does either. A user server named `tessl-proxy` survives whatever command it runs. An entry keyed `tessl` that fails the predicate — a wrapper command, an absolute path, an extra argument, an `env` block — is retained byte for byte and blocks with `mcp-ambiguous`; a config that does not parse is retained and blocks with `mcp-malformed-config`. There is no `--retire-mcp` flag and no force path: the remedy is to remove the entry yourself and re-run.

Inventory and planning read the project separately, and planning's read is what the transaction accepts as its before-image. Every structured host is therefore read exactly once during planning, and the Tessl entry is re-classified against those bytes before any selector is composed — including the Codex host where the hook dispatchers and the MCP entry splice together. An entry that changed since it was inventoried blocks with `mcp-ownership-changed` and a re-inventory remedy; a shared link is bound the same way.

An MCP config must hold exactly one document. Trailing values or garbage after it block, with or without a Tessl member in that first object: a client that cannot read its own config is not a state finalization may leave behind. A malformed supported config — including `.codex/config.toml`, which is also a hook host — blocks with `mcp-malformed-config` and a sanitized parse coordinate, raised before realization reaches the same file. Every JSON coordinate is a byte offset from the start of the file; a truncated document reports the offset where the input ends.

The same canonical object can be written where the removal cannot address it — as an inline table under a parent header, with no header of its own and no per-field location to splice. That layout blocks with `mcp-unsupported-representation` rather than failing while planning.

The removal is a `structured-entry` splice inside the same transaction as every other edit. In TOML the header token and the assignments are removed as disjoint edits: the canonical object proves ownership of the integration, not of a comment somebody wrote on the header line, between two fields, trailing an assignment, or inside a value that spans several lines. Every such comment survives with its exact bytes. When it takes the last content ACR does not own out of a shared host, finalization records the target as `generated-only` and reports the change in `reanchored[]`: a ledger left at shared ownership would make every later `acr check` and `acr realize` refuse the merge for want of unmanaged content to preserve. The same applies to a Markdown host — a host ACR generated can hold nothing but ACR's own block once a later Tessl span is retired — decided through the Markdown ownership parser, binding each block to a ledger managed hash. Emptiness is byte presence, so a host left holding one separator line stays shared.

An ownership change moves a target into or out of the local Git exclusion block, so finalization writes that block in the same transaction, through one confined edit at the location Git resolves. The dry run reports it as a `git-exclusion` note. A finalization that leaves this to the next realization is not finished: `acr check` must be clean immediately afterwards. Every foreign member and unknown field survives byte for byte; in TOML the table header is removed with its fields, and a comment written between that table and the next is preserved.

Diagnostics about an MCP entry report the config path, the container, the server key, the **names** of the fields present, a reason code naming the field that failed the predicate, and a `sha256:` digest over a canonical serialization of the whole entry. They report no field value: not an `env` value, not an `args` element, not the `command` string. An MCP entry can carry a credential, and these records reach stdout, the JSON envelope and whatever CI log captures them.

Before the first mutation ACR inventories every target and stages all before-images in `.agents/.acr-transactions/`. Removal entries include hash, size, mode, symlink target, and operation `remove`; live removals are renamed into the journal's `removed/` area, with the verified before-image used as the fallback on a cross-device rename. Content is checked again before mutation and `tessl.json` is checked immediately before its last removal. Any drift is a conflict and triggers rollback. The result contains sorted `removed[]`, `retained[]`, `reanchored[]`, and `staleReferences[]` arrays; removed native rows and matching stale references include the ACR replacement path. The stale scan is limited to Git-tracked regular non-binary files, never follows symlinks, and does not flag retained `tessl mcp start` commands. `--dry-run` computes the same plan with `wrote:false`.

Inside a Git repository, both `tessl.json` and every vendor file must already be tracked. An untracked manifest names the exact remedy `git add tessl.json && git commit`. Outside Git, this check is vacuous and the report notes no version-control assumption. After success, rerunning migration or finalization reports no Tessl installation with `wrote:false`.

## Coexistence and duplicate effects

ACR adds its owned Markdown blocks and structured hook entries beside Tessl's entries. It does not absorb or suppress Tessl configuration. As a result, a hook supplied by both systems can execute twice. Every such canonical event produces this mandatory warning with both launch paths:

```text
WARNING duplicate-effect session-start: tessl hook run --event=session-start + .claude/hooks/acr__…/session-start.sh
```

This warning lasts until finalization. Make side-effecting hooks idempotent, or defer migration if duplicate network calls, comments, prompts, or local mutations would be unsafe.

The coexistence report also identifies ACR-owned output, positively identified Tessl-owned paths frozen for this run, unmanaged fragments preserved in shared hosts, semantic diffs, uncovered agents, stale journal staging, and state paths hidden by Tessl's `.gitignore` block.

Migration adds the packages it maps to the ACR state a project already has. A declaration the mapping does not name keeps its requested policy, hold and extension fields, its lock keeps its release, commit, package version and content hash, and its realized output stays in place; nothing about it is re-resolved, updated or reinstalled. The one exception is a vendored source the same run supersedes with an upstream one, whose tree that run removes. A mapping that would move a package the project already declares to a different request, and a state that selects different agents, still exit `1` with `project_state_conflict` and write nothing.

## Crash recovery and concurrency

Mutating realization commands use a non-blocking `flock` at `.agents/.acr-transactions/.lock`, recover a pending schemaVersion 1 journal before planning (the journal's own version, unrelated to the realization ledger's), and stage complete before-images before the first target rename. The journal covers native output, Git exclusion state, `agents.yaml`, and `.agents/registry.lock`. A target matching the journal after-state is restored; one already at its before-state is left alone; anything else is `recovery_conflict` and is preserved for manual reconciliation.

Dry-run and check never create a claim or recover. A canonical journal is `pending_transaction`; a `.staging-*` directory is reported as `stale_transaction_staging` and remains non-blocking. Unsupported journal versions fail closed. The project filesystem must provide working advisory `flock`; `transaction_lock_unavailable` has no unsafe fallback.

Human-readable output goes to stdout. `--json` writes one success envelope whose `command` is `migrate` and whose `result` is the schemaVersion 2 report below. A blocked finalization carries the same report in the failure envelope's `result`, so exit `4` names its cause instead of requiring two runs to be diffed. Diagnostics stay on stderr.

## Inputs

The inventory reads:

- `tessl.json`
- `.tessl/plugins/<workspace>/<package>/{.tessl-plugin/plugin.json,tile.json,tessl-package.json}` and the package `rules/`, `skills/`, and `hooks/` trees
- `.tessl/RULES.md`
- native agent outputs Tessl wrote under `.claude`, `.codex`, `.cursor`, `.gemini`, `.github`, `.vscode`, `.openhands`, and the shared `.agents/skills` surface
- MCP server maps in `.mcp.json`, `.cursor/mcp.json`, `.codex/config.toml`, `.vscode/mcp.json`, `.github/mcp.json`, and `.gemini/settings.json`

`plugin.json` is authoritative when both manifests exist. A path disagreement between the two is `ambiguous`. A rule or skill declared by only one manifest is not: a stale `tile.json` that omits a newly added `plugin.json` rule does not taint that rule. `tile.json` cannot express hooks, so plugin-declared hooks are not a disagreement.

## Ownership markers

Tessl-owned content is recognized by:

- Markdown heading suffix `<!-- tessl-managed -->` (no closing marker)
- RULES.md includes `@plugins/<workspace>/<package>/rules/<id>.md`
- structured ledgers `tessl.hooks."<workspace>/<package>"` and `tessl.native."<workspace>/<package>"`
- dispatcher `tessl hook run --plugin-path=… --event=… --agent=… --schema-version=1` at the head of the command
- native names `tessl__<skill-id>` and `.cursor/rules/tessl__rule__<workspace>__<package>__<rule-id>.mdc`
- gitignore block `# === Tessl-generated artifacts (managed by …) ===` … `# === end Tessl-generated artifacts ===`
- `${TESSL_PLUGIN_DIR}` in plugin hook commands

Native `tessl__*` skill paths are usually symlinks into the plugin tree. The inventory records those paths in `natives[]` and never follows them; `RootSnapshot` rejects a symlink at the leaf.

## Normalization

Rules take activation from the plugin-tree source file, not the Cursor `.mdc`. Tessl prepends `alwaysApply: true` onto every `.mdc`, including source files whose frontmatter says `alwaysApply: false`. `alwaysApply: true` maps to `activation.mode: always`. `alwaysApply: false` plus `applyTo:` (aliases `globs:`, `paths:`) maps to `mode: paths` with the comma-split glob half of `"<globs> — <prose>"`. The prose half and `description:` have no v1 field and are recorded in `lossy`.

The same `.mdc` is a drift detector: strip exactly one frontmatter document plus one separator newline; the remainder must equal the source bytes.

Skills are directory artifacts. The digest is `sha256` over the sorted `(relative POSIX path, exec bit, content hash)` triples of the plugin-tree files. Sibling scripts stay inside the skill directory. Tessl has no standalone script class, so `artifacts.scripts` from this inventory is empty.

Hooks come from `plugin.json` `hooks` and `nativeHooks`, never from native dispatcher entries. Command parsing is a closed two-form grammar: `{command, args[]}` whose `args[0]` is `${TESSL_PLUGIN_DIR}/<relpath>`, or `bash "${TESSL_PLUGIN_DIR}/<relpath>"`. Native event spellings map through the [adapter vocabulary](adapters.md). Entries identical across agents collapse to one logical hook. IDs are the script basename minus extension.

The comparable object for #2 and #8 is the sorted list of `(package, kind, id, activation, event, digest)`. It contains no native filename.

## Classification

Package `packageMapping` is `github-mapped` only when the plugin manifest states an explicit `repository`. The inventory never derives a GitHub URL from the Tessl package name. Absent evidence, mapping is `unmapped` and `mappingCandidate` carries `github:<tessl-identity>` as a hint for #2's `--map`. `tesslIdentity` stays beside `name` because a mapped ACR identity can differ from the Tessl identity.

| Kind | `migratable` | `ambiguous` | `unsupported` |
| --- | --- | --- | --- |
| rule | declared, readable, activation parses | manifests disagree on path; `.mdc` drift; `applyTo:` with no parsable glob half; RULES.md names a missing file | — |
| skill | declared, `SKILL.md` present, tree readable | two packages claim one `tessl__<id>`; native copy diverges from the plugin tree; declared with `SKILL.md` absent (`missing-skill`) | tree escapes the project root. A skill that is both unsupported and duplicated stays unsupported. |
| hook | command matches the grammar, event in v1 | per-agent entries differ in command body, not just spelling | event outside v1; command outside the grammar |

A declared `SKILL.md` that exists but cannot be read fails the inventory. `missing-skill` is the absent-file case. A declared rule or skill path that is not a package-relative POSIX path, and a native JSON or TOML config that cannot be decoded, also fail the inventory.

`unmapped` paths have no v1 home: `.tessl/RULES.md`, the gitignore Tessl block, `tessl-package.json`, `.tessl/plugins/**` files unreachable from a declared artifact, symlinked entries under `.tessl/plugins/**` (`plugin-symlink`), and orphan `tessl__*` natives under adapter rule and skill roots that no installed package claims (`orphan-tessl-native`).

Shared skill entries and MCP servers are classified per entry in `sharedSkills[]` and `mcp[]`, never by file presence.

`preserved` reuses the #6 include graph and records unmanaged Markdown prefixes, user hooks beside Tessl dispatchers, `.claude/settings.local.json`, and extra files in a copied native skill directory. User-authored native rules and skills beside Tessl natives are not enumerated. Include-graph nodes under a package root and `.tessl/RULES.md` itself are dropped before that pass: they are already artifacts or `unmapped`, never user content. Extra content inside a `<!-- tessl-managed -->` heading span (through the next same-or-higher heading, or EOF) is `ambiguous` and retained.

A non-empty `lossy` list means the normalized configurations are not equivalent. That blocks #8 finalization and leaves #2 coexistence unblocked.

## Report shape

`schemaVersion` is `2`. The payload is Go structs, never `map[string]any`. Packages sort by `name`, artifacts by `(kind, id)`, and every path slice POSIX-lexically. Version 2 adds `sharedSkills[]`, `mcp[]`, and the migration report's `blockers[]`.

```json
{
  "ok": true,
  "command": "migrate",
  "result": {
    "schemaVersion": 2,
    "dryRun": true,
    "wrote": false,
    "agents": [
      {"id": "claude-code", "covered": true, "evidence": [".claude/settings.json", ".claude/skills/"]},
      {"id": "gemini", "covered": false, "evidence": [".gemini/settings.json"]}
    ],
    "packages": [
      {
        "name": "example/alpha",
        "tesslIdentity": "example/alpha",
        "version": "1.0.0",
        "manifest": "plugin.json",
        "packageMapping": "unmapped",
        "mappingCandidate": "github:example/alpha",
        "artifacts": [
          {
            "id": "always-rule",
            "kind": "rule",
            "classification": "migratable",
            "activation": {"mode": "always"},
            "digest": "sha256:…",
            "natives": [".cursor/rules/tessl__rule__example__alpha__always-rule.mdc"]
          }
        ]
      }
    ],
    "sharedSkills": [
      {
        "path": ".agents/skills/tessl__review-change",
        "kind": "symlink",
        "target": "../../.tessl/plugins/example/alpha/skills/review-change",
        "skillId": "review-change",
        "package": "example/alpha",
        "disposition": "removable"
      }
    ],
    "mcp": [
      {
        "path": ".mcp.json",
        "container": "mcpServers",
        "key": "tessl",
        "fields": ["args", "command", "type"],
        "digest": "sha256:…",
        "disposition": "canonical",
        "reason": "canonical-tessl-entry"
      }
    ],
    "preserved": [{"path": "AGENTS.md", "reason": "unmanaged-prefix"}],
    "unmapped": [{"path": ".tessl/RULES.md", "reason": "tessl-index"}],
    "ambiguous": [],
    "unsupported": []
  }
}
```

`agents[]` lists Tessl per-agent native trees that have evidence. ACR currently covers `claude-code`, `codex`, and `cursor`; `gemini`, `github`, `vscode` and `openhands` are uncovered. An uncovered agent is an unsupported *target*, not an unsupported artifact. #2's equivalence gate is the covered intersection. `.agents/skills` is not in this list: it is a surface with computed coverage, reported in `sharedSkills[]`.

`.agents/` is shared: Tessl writes `.agents/skills/tessl__*`, ACR writes `.agents/skills/acr__*` and owns `.agents/registry.lock` and `agents.yaml`. Finalization works by positively identified file, never by directory. `.agents/registry.lock`, `.agents/vendor/**` and `.agents/.acr-transactions/**` stay unreachable from every producer: an adapter target under `.agents` is refused as before, and the shared surface goes through a separate predicate that accepts only `.agents/skills/acr__…`.

Text output groups migratable, ambiguous, and unsupported artifacts under each package and prints the agent-coverage table plus project-level preserved, unmapped, ambiguous, and unsupported paths. Artifact classification never uses `unmapped`; that bucket is project-level only.

## Deferred

- #11 — migrate a vendored package into a publishable ACR-native repository
