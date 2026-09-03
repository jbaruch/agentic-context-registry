# Tessl-to-ACR migration guide

This is the complete adoption path for a Tessl plugin producer and each consumer project. Follow stages 0–7 in order. Each stage says what must already be true, what the command changes, what is on disk afterward, and how a refusal is retried. The detailed ownership vocabulary lives in the [migration reference](migration.md); this guide supplies the journey.

## Stage 0: prepare and publish the producer

Start in every Tessl plugin repository used by the consumer. GitHub authentication comes first: ACR reads `GH_TOKEN`, then `GITHUB_TOKEN`, then `gh auth token`, then `git credential fill`. Public sources require no token. GitHub deliberately presents a private repository without usable credentials as a 404, “not found or inaccessible”; authenticate with `gh auth login` or set a token that can read the repository.

Convert the producer before touching a consumer:

```console
$ acr migrate tessl-plugin --dry-run
# fixture: producer
# exit: 0
Would convert plugin.json → agent-plugin.yaml
package: example/alpha 1.0.0
artifacts: 1
```

Review the proposed `agent-plugin.yaml`, rerun without `--dry-run`, commit it beside `.tessl-plugin/plugin.json`, then create and push the version tag. Rehearse the immutable publication before uploading assets:

```console
$ acr publish --dry-run
# fixture: publisher
# exit: 0
Release v1.0.0 is publishable with 3 assets; rerun without --dry-run to upload it.
```

Producer conversion can refuse [`unknown_field`](troubleshooting.md#missing-source-mappings-and-migration), [`unmapped_field`](troubleshooting.md#missing-source-mappings-and-migration), [`agent_widening`](troubleshooting.md#missing-source-mappings-and-migration), [`ambiguous_manifest`](troubleshooting.md#missing-source-mappings-and-migration), or [`manifest_conflict`](troubleshooting.md#missing-source-mappings-and-migration). Publication can refuse [`no_publishable_tag`](troubleshooting.md#commands-publishing-freshness-and-transactions), [`tag_version_mismatch`](troubleshooting.md#commands-publishing-freshness-and-transactions), or [`tag_not_pushed`](troubleshooting.md#commands-publishing-freshness-and-transactions). Apply the linked row's remedy and repeat the relevant dry-run before continuing.

The [dual-publishing contract](publishing.md#dual-publishing) is the sole reference for maintaining both manifests and using one tag. The required order is producer conversion (#11) and then immutable ACR publication (#9); a consumer cannot map a repository that has no ACR package manifest and release.

On disk now: each producer has a committed `agent-plugin.yaml`; its original Tessl manifest and artifact sources remain; the version tag is available as a GitHub Release.

## Stage 1: inventory a fresh consumer

Start in the consumer checkout with its existing `tessl.json`, `.tessl/` installation, and native Tessl output. Do **not** run `acr init`: migration derives the selected agent set from Tessl evidence and synthesizes `agents.yaml` and `.agents/registry.lock` itself.

Run the read-only inventory and mapping rehearsal:

```console
$ acr migrate tessl --dry-run --map example/alpha=github:example/alpha@v1.0.0
# fixture: tessl-consumer
# exit: 0
Coexistence dry-run. Finalization: ready

Tool-owned
  .agents/registry.lock  path
  .agents/registry.lock  state
  .codex/config.toml  path
  .codex/config.toml  structured-entry freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  file freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  path
  AGENTS.md  managed-block package-rules-079695046314d5b347dec80132e12540fa3ae12e7722722ebbf7aed212a13e6a
  AGENTS.md  path
  agents.yaml  path
  agents.yaml  state

Tessl-owned (frozen)
  .tessl/RULES.md  path
  .tessl/plugins/example/alpha  package
  tessl.json  manifest

Unmanaged (preserved)
  (none)

Effective differences
  (none)
```

The result must report no unexpected effective differences. `--dry-run` writes nothing. If nonempty pre-existing ACR state selects different agents or dependencies, the command exits `1` with `project_state_conflict`; run `acr migrate tessl --dry-run --json`, align or remove the disagreeing `agents.yaml` or `.agents/registry.lock`, and retry. Empty or absent state is synthesized, so prior initialization is neither required nor desirable.

On disk now: nothing changed; the report shows the ACR state and native plan that coexistence would create.

## Stage 2: resolve missing mappings

ACR never guesses a repository from a Tessl package name. A package without repository evidence exits `1` with `unmapped_package` and writes nothing:

```console
$ acr migrate tessl --dry-run
# fixture: tessl-unmapped
# exit: 1
```

Retry with an explicit mapping. `FROM` is the Tessl workspace/package identity; the right side is a canonical GitHub source and optional release or commit request:

```console
$ acr migrate tessl --dry-run --map example/alpha=github:example/alpha@v1.0.0
# fixture: tessl-unmapped
# exit: 0
Coexistence dry-run. Finalization: ready

Tool-owned
  .agents/registry.lock  path
  .agents/registry.lock  state
  .codex/config.toml  path
  .codex/config.toml  structured-entry freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  file freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  path
  AGENTS.md  managed-block package-rules-079695046314d5b347dec80132e12540fa3ae12e7722722ebbf7aed212a13e6a
  AGENTS.md  path
  agents.yaml  path
  agents.yaml  state

Tessl-owned (frozen)
  .tessl/RULES.md  path
  .tessl/plugins/example/alpha  package
  tessl.json  manifest

Unmanaged (preserved)
  (none)

Effective differences
  (none)
```

For many mappings, commit a YAML file and pass `--mapping-file PATH`. CLI `--map` values take precedence over the file, which takes precedence over repository evidence in the package manifest. `mapping_conflict`, `mapping_file_invalid`, `tessl_version_unavailable`, and `ambiguous_tessl_version` all write nothing; use the exact retry in [Troubleshooting](troubleshooting.md#missing-source-mappings-and-migration).

On disk now: still unchanged; every migratable package has an explicit, reviewable ACR source.

## Stage 3: apply coexistence

Apply exactly the mapping that passed dry-run:

```console
$ acr migrate tessl --map example/alpha=github:example/alpha@v1.0.0
# fixture: tessl-consumer
# exit: 0
Coexistence applied. Finalization: ready

Tool-owned
  .agents/registry.lock  path
  .agents/registry.lock  state
  .codex/config.toml  path
  .codex/config.toml  structured-entry freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  file freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  path
  AGENTS.md  managed-block package-rules-079695046314d5b347dec80132e12540fa3ae12e7722722ebbf7aed212a13e6a
  AGENTS.md  path
  agents.yaml  path
  agents.yaml  state

Tessl-owned (frozen)
  .tessl/RULES.md  path
  .tessl/plugins/example/alpha  package
  tessl.json  manifest

Unmanaged (preserved)
  (none)

Effective differences
  (none)
```

This one journaled transaction writes `agents.yaml`, `.agents/registry.lock`, and ACR-owned native files or managed entries. It never edits `tessl.json`, `.tessl/**`, `tessl__*` native output, Tessl-owned host spans or objects, or the Tessl-managed `.gitignore` span. During coexistence, equivalent Tessl and ACR hooks can both fire; a `duplicate-effect` notice names each pair.

A `gitignored_state` notice names the ignored ACR path and the exact `<file>:<line>` rule. ACR never applies the remedy. Un-ignore `agents.yaml` and `.agents/registry.lock` before committing: if the lock remains ignored, a fresh clone has to resolve mutable `latest` requests again and can select different commits. Generated-only native output may remain covered by ACR's local `.git/info/exclude` block.

On disk now: Tessl remains fully installed and byte-identical; ACR state and equivalent ACR-native output coexist beside it.

## Stage 4: verify coexistence

Check the selected adapters after applying:

```console
$ acr check --agent codex
# fixture: tessl-ready
# exit: 0
Realization is current for codex.
```

Then inspect `acr list`, review both state files, exercise the coding agents, and commit the shared files and state that policy requires. `acr check` exits `3` for a safe unapplied plan and `4` for an ownership refusal. Use `acr realize --dry-run` before applying a repair. Do not delete Tessl yet; first confirm that rules, skills, scripts, and hooks have equivalent behavior.

On disk now: coexistence is verified and its ACR state is committed, while Tessl still provides an immediate fallback.

## Stage 5: vendor an unmapped package when necessary

If no publishable repository exists, vendor instead of inventing one:

```console
$ acr migrate tessl --vendor-unmapped --dry-run
# fixture: tessl-unmapped
# exit: 0
Coexistence dry-run. Finalization: ready

Vendored packages
  vendor:example/alpha  .agents/vendor/example/alpha  sha256:81361af9a7ca2ed3c7c1a60d0be09dd70c937de200c8b6691939e30091ddf0e8

Tool-owned
  .agents/registry.lock  path
  .agents/registry.lock  state
  .codex/config.toml  path
  .codex/config.toml  structured-entry freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  file freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  path
  AGENTS.md  managed-block package-rules-7f6107c52799dbf2b867d54d4a5b9e58a6c7930673eedd727f233b505679b274
  AGENTS.md  path
  agents.yaml  path
  agents.yaml  state

Tessl-owned (frozen)
  .tessl/RULES.md  path
  .tessl/plugins/example/alpha  package
  tessl.json  manifest

Unmanaged (preserved)
  (none)

Effective differences
  (none)
```

Review the destination `.agents/vendor/<workspace>/<package>`, then rerun without `--dry-run`. ACR copies only verified regular files, records a `vendor:` declaration and lock, and realizes from that immutable local tree. Commit the vendor tree and explicitly un-ignore it if needed. Direct `install`, `update`, or `resume` of a `vendor:` source is refused; later replace it with an equivalent GitHub package by rerunning migration with `--map`.

On disk now: otherwise-unmapped package bytes and their lock evidence are reproducible inside `.agents/vendor`, with the Tessl installation still untouched.

## Stage 6: rehearse rollback and recovery

There are three independent rollback boundaries. Read the safety contract's [dependency hold barrier](safety.md#dependency-hold-barrier), [journal recovery](safety.md#journal-recovery), and [migration undo](safety.md#migration-undo) before finalization.

For a bad release of a dependency declared as `latest`, rehearse the explicit temporary hold:

```console
$ acr install github:example/alpha@v1.0.0 --hold --dry-run
# fixture: github-newer
# exit: 0
install would update dependency state; rerun without --dry-run to write agents.yaml and .agents/registry.lock.
Held behind a rollback barrier: github:example/alpha.
```

Without `--hold` or `--pin`, a rollback refuses with `downgrade_choice_required`. A hold preserves `latest` behind its rejected-release barrier; only `acr resume SOURCE` moves forward again. A complete journal is recovered by retrying the original mutating command. Neither mechanism restores Tessl after finalization; that requires committed Tessl evidence.

On disk now: the real project is unchanged by this rehearsal, and the operator has verified the dependency, transaction, and migration recovery procedures.

## Stage 7: finalize and remove Tessl-owned output

Before finalizing, commit `tessl.json`, any vendored source, ACR state, and shared native/config files. Run coexistence once more until it is current, then preview deletion:

```console
$ acr migrate tessl --finalize --dry-run --map example/alpha=github:example/alpha@v1.0.0
# fixture: tessl-ready
# exit: 0
Tessl finalization dry-run. Removed: 5; retained: 0

Removed (rollback)
  delete  .tessl/RULES.md  sha256:1b91b877fcdf100880f19c36a4d553538166847a64fdd408d25c03f3a5a0ead6
  delete  .tessl/plugins/example/alpha/.tessl-plugin/plugin.json  sha256:d2ff16943084693aa7196d8e35e34a715dbbe7d71578f3348ddd0517e4f4466c
  delete  .tessl/plugins/example/alpha/rules/always.md  sha256:0d27e6ca0cab3116cb162ce773e30af203f1f192dc880a7bad7a612fb42f857e
  delete  .tessl/plugins/example/alpha/tile.json  sha256:0f207e08919b52387afe7a3b7644497dd7d87db26dc44aa028d3f57d39376058
  delete  tessl.json  sha256:1e803810bc95587d2b50bb2c9b2ccd63f49931ba40de772f67bb336ba5223204

Retained Tessl output
  (none)

Stale tracked references
  (none)
  Scan is limited to Git-tracked files; out-of-repository references cannot be detected.

Tool-owned
  .agents/registry.lock  state
  .codex/config.toml  structured-entry freshness-session-start
  .codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh  file freshness-session-start
  AGENTS.md  managed-block package-rules-079695046314d5b347dec80132e12540fa3ae12e7722722ebbf7aed212a13e6a
  agents.yaml  state

Tessl-owned (frozen)
  .tessl/RULES.md  path
  .tessl/plugins/example/alpha  package
  tessl.json  manifest

Unmanaged (preserved)
  .codex/config.toml  fragment
  AGENTS.md  fragment

Effective differences
  (none)
```

`finalization_blocked` exits `4` until mappings, equivalence, recoverability, ambiguity, and adapter coverage are proven. Apply any structured `remedy`, rerun the same dry-run, then remove Tessl-owned output by dropping `--dry-run`. Finalization removes only positively identified Tessl files, managed spans, and objects; preserves unrelated siblings and unsupported configuration; re-anchors affected ledger `OutputHash` values; removes `tessl.json` last and writes `.agents/registry.lock` last.

Finalize may splice the Tessl-generated span out of `.gitignore`. If vendor evidence is ignored, the refusal prints `/.agents/*` plus `!/.agents/vendor/` as a suggested remedy; ACR never writes those lines. To undo finalization, restore committed `tessl.json` and vendor files with `git checkout`, restore other committed splices named by the report, and run `tessl install`.

On disk now: proven Tessl-owned output is gone, retained items are listed, ACR owns only its recorded state and native entries, and Git history contains the evidence needed to restore Tessl.
