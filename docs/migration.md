# Tessl migration

`acr migrate tessl` converts an existing Tessl consumer project to ACR while leaving Tessl installed. It normalizes Tessl artifacts onto the v1 `agent-plugin.yaml` model, resolves mapped ACR packages, realizes Claude Code, Codex, and Cursor output, and reports whether later Tessl removal would preserve effective behavior. Deleting Tessl remains issue #8.

## Command

```text
acr migrate tessl [--mapping-file PATH] [--map FROM=github:owner/repository[@REQUESTED]] [--dry-run] [--finalize] [--json] [--project PATH]
```

`--dry-run` performs resolution, materialization, semantic comparison, and planning but writes nothing. Without it, ACR writes only ACR-owned files and state in coexistence mode. `tessl.json`, `.tessl/**`, `tessl__*` natives, Tessl-managed host spans and objects, and the Tessl `.gitignore` block remain byte-identical. ACR never hosts a managed block inside a Tessl-owned path.

`--finalize` never applies coexistence or deletes Tessl. It exits `4` with `finalization_blocked` while any effective diff, lossy mapping, unmapped package, ambiguous artifact, or uncovered agent remains. When all gates pass it exits `1` with `not_implemented` until issue #8 is installed.

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

An unmapped package blocks the entire run. Add an explicit mapping rather than accepting the displayed `mappingCandidate`, which is only a hint.

## Coexistence and duplicate effects

ACR adds its owned Markdown blocks and structured hook entries beside Tessl's entries. It does not absorb or suppress Tessl configuration. As a result, a hook supplied by both systems can execute twice. Every such canonical event produces this mandatory warning with both launch paths:

```text
WARNING duplicate-effect session-start: tessl hook run --event=session-start + .claude/hooks/acr__…/session-start.sh
```

This warning lasts until issue #8 finalizes the migration. Make side-effecting hooks idempotent, or defer migration if duplicate network calls, comments, prompts, or local mutations would be unsafe.

The coexistence report also identifies ACR-owned output, positively identified Tessl-owned paths frozen for this run, unmanaged fragments preserved in shared hosts, semantic diffs, uncovered agents, stale journal staging, and state paths hidden by Tessl's `.gitignore` block.

## Crash recovery and concurrency

Mutating realization commands use a non-blocking `flock` at `.agents/.acr-transactions/.lock`, recover a pending schemaVersion 1 journal before planning, and stage complete before-images before the first target rename. The journal covers native output, Git exclusion state, `agents.yaml`, and `.agents/registry.lock`. A target matching the journal after-state is restored; one already at its before-state is left alone; anything else is `recovery_conflict` and is preserved for manual reconciliation.

Dry-run and check never create a claim or recover. A canonical journal is `pending_transaction`; a `.staging-*` directory is reported as `stale_transaction_staging` and remains non-blocking. Unsupported journal versions fail closed. The project filesystem must provide working advisory `flock`; `transaction_lock_unavailable` has no unsafe fallback.

Human-readable output goes to stdout. `--json` writes one success envelope whose `command` is `migrate` and whose `result` is the schemaVersion 1 report below. Diagnostics stay on stderr.

## Inputs

The inventory reads:

- `tessl.json`
- `.tessl/plugins/<workspace>/<package>/{.tessl-plugin/plugin.json,tile.json,tessl-package.json}` and the package `rules/`, `skills/`, and `hooks/` trees
- `.tessl/RULES.md`
- native agent outputs Tessl wrote under `.claude`, `.codex`, `.cursor`, `.gemini`, `.github`, `.vscode`, and `.agents/skills`

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

`unmapped` paths have no v1 home: `.tessl/RULES.md`, the gitignore Tessl block, `tessl-package.json`, `.tessl/plugins/**` files unreachable from a declared artifact, symlinked entries under `.tessl/plugins/**` (`plugin-symlink`), and orphan `tessl__*` natives under adapter rule and skill roots that no installed package claims (`orphan-tessl-native`). MCP servers (`.mcp.json`, `.cursor/mcp.json`, `[mcp_servers.tessl]`, `.gemini/settings.json`, `.vscode/mcp.json`) are `unsupported`.

`preserved` reuses the #6 include graph and records unmanaged Markdown prefixes, user hooks beside Tessl dispatchers, `.claude/settings.local.json`, and extra files in a copied native skill directory. User-authored native rules and skills beside Tessl natives are not enumerated. Include-graph nodes under a package root and `.tessl/RULES.md` itself are dropped before that pass: they are already artifacts or `unmapped`, never user content. Extra content inside a `<!-- tessl-managed -->` heading span (through the next same-or-higher heading, or EOF) is `ambiguous` and retained.

A non-empty `lossy` list means the normalized configurations are not equivalent. That blocks #8 finalization and leaves #2 coexistence unblocked.

## Report shape

`schemaVersion` is `1`. The payload is Go structs, never `map[string]any`. Packages sort by `name`, artifacts by `(kind, id)`, and every path slice POSIX-lexically.

```json
{
  "ok": true,
  "command": "migrate",
  "result": {
    "schemaVersion": 1,
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
    "preserved": [{"path": "AGENTS.md", "reason": "unmanaged-prefix"}],
    "unmapped": [{"path": ".tessl/RULES.md", "reason": "tessl-index"}],
    "ambiguous": [],
    "unsupported": [{"path": ".cursor/mcp.json", "reason": "mcp-server"}]
  }
}
```

`agents[]` lists Tessl native trees that have evidence. ACR currently covers `claude-code`, `codex`, and `cursor`. An uncovered agent is an unsupported *target*, not an unsupported artifact. #2's equivalence gate is the covered intersection.

`.agents/` is shared: Tessl writes `.agents/skills/tessl__*`; ACR owns `.agents/registry.lock` and `agents.yaml`. #8 must finalize by positively identified file, never by directory.

Text output groups migratable, ambiguous, and unsupported artifacts under each package and prints the agent-coverage table plus project-level preserved, unmapped, ambiguous, and unsupported paths. Artifact classification never uses `unmapped`; that bucket is project-level only.

## Deferred

- #8 — Tessl deletion, vendoring unmapped packages, and the final rollback report
