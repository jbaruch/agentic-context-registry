# Producer conversion from Tessl plugin manifests

`acr migrate tessl-plugin [PATH]` offers two producer modes. The default adds `agent-plugin.yaml` for dual distribution and preserves source files. `--acr-only` plans and applies a supported clean conversion, including target identity and retirement of selected Tessl producer metadata. PATH is the plugin package root and defaults to `.`.

```text non-executable
acr migrate tessl-plugin [PATH] [--dry-run] [--json] [--repository URL] [--acr-only [--package-version SEMVER]] [--accept-agent-widening]
```

Consumer inventory (`acr migrate tessl`) is a separate command. This page covers only producer conversion.

For the default mode, follow the [dual-publishing contract](publishing.md#dual-publishing). Clean mode uses the explicit tag sequence below.

## Clean ACR-only conversion

Choose the target repository in the command. A clone URL ending in `.git` is normalized to the repository URL. Its `OWNER/REPO` becomes the ACR package identity even when the original Tessl name differs. The source version is preserved unless `--package-version` overrides it. Neither identity nor version needs a post-conversion manifest edit.

```text non-executable
acr migrate tessl-plugin plugins/example --acr-only --repository https://github.com/OWNER/REPO --package-version 1.2.3 --dry-run --json
acr migrate tessl-plugin plugins/example --acr-only --repository https://github.com/OWNER/REPO --package-version 1.2.3 --json
```

Review the dry-run's complete before/after file contents, diffs, removals, permission bits, identity and version, and `publishedFiles`. Apply the same command without `--dry-run`. Commit the resulting repository, create and push `v1.2.3`, and run `acr publish` at the repository root, or use the translated tag workflow. The command executes no package scripts, agents, model services or publishing operations.

The enclosing Git checkout is the output boundary; outside Git, the selected directory is the boundary. The converter writes root `agent-plugin.yaml` and retains nested package files with prefixed artifact paths. Multiple authored packages, competing ACR manifests, symlinks in selected producer/workflow paths and path escapes refuse. Unrelated repository symlinks are preserved without traversal. The shared Tessl parser still validates artifact IDs, hooks, activation and manifest disagreements.

Clean conversion supports these deterministic changes:

- Rewrite complete references to existing files in declared skill trees into repository-relative paths, including owned `.tessl/plugins/<source-identity>/...` references and cross-skill links. It uses the same boundary scanner as native realization. Missing files, directory-only references, dynamic suffixes, emphasis without code delimiters, quoted interiors, opaque structured contexts, redirections and other unsupported owned paths refuse in all authored files. Standalone rule/hook references also refuse because native rebasing has no path contract for those artifacts. Hook paths use the shared closed parser; owned runtime paths in hook arguments require semantic conversion. Foreign references and attribution remain unchanged.
- Retire only the selected `.tessl-plugin/plugin.json`, `tile.json`, `.tesslignore` and `.tileignore` files when present. Empty directories stay. Retired ignore entries are listed in notes with literal, directory and simple-glob matches against the publication inventory; other ignore syntax needs review against the complete `publishedFiles` list. Original name, repository, homepage, author and license metadata becomes comments in the distributed manifest. Root agent instructions, consumer `tessl.json`, install state, foreign installed content and unrelated files stay untouched.
- Preserve source permission bits, including executable status. A planned fresh regular file with mode `000` refuses as `unsupported_file_mode` before a successful plan or any writes: ACR cannot preserve verification access on the new inode. Semantic validation also refuses before staging any retained regular file at that mode. The original source stays intact; selecting an agent does not repair or retry this limitation. Unchanged readable `000` files in deterministic conversion and intentional deletion of readable `000` metadata remain supported. Ordinary read-only files and traversable read-only directories remain supported. Publication normalizes permissions through Git to executable `0755` and ordinary `0644`; installation and native materialization preserve those statuses. No consumer `chmod` or interpreter-prefix repair is required.
- Preserve license and notice bytes. A license/notice outside `manifest.PackageFiles` blocks conversion with its path: ACR needs a support-file packaging contract before such a package can be converted cleanly.

### Recognized publication workflow

The supported form has `on.push.branches: [main]`, optional name and contents/pull-requests write permissions, and one Tessl job on `ubuntu-latest` containing only `actions/checkout@v4` with no options followed by `tesslio/patch-version-publish@v1`. Publisher inputs are `token: ${{ secrets.TESSL_TOKEN }}` and a literal `path` equal to the selected package (`.` may be omitted). Optional step/job names are accepted. YAML aliases, merges, duplicate keys, additional steps, other publisher inputs and custom workflow logic refuse.

The converter creates `.github/workflows/acr-publish.yml`, triggered by `v*` tags, calling the ACR reusable workflow at commit `d3bc96b33b42293aecd1702c04aa94513a3dab1b` with explicit `path: .` and `acr-version: v0.1.6`. Renew these pins after stable ACR releases with workflow contract verification. This deliberately changes automatic patch publication on main into explicit version tags. The old publication job is removed; independent test jobs retain their exact bytes and original trigger. A workflow containing only that publisher is retired. Dependent jobs, unknown Tessl logic, conflicting output, review gates and skill-review thresholds have no automatic translation.

### Transactions and repeat application

Planning reads source manifests, maps artifacts and validates complete metadata and file inventory through the same opened root. Custom symlinks anywhere in the selected path refuse, including outside Git; ordinary macOS `/var`, `/tmp` and `/etc` anchors are recognized. This protects coherent planning and retains source revalidation, without promising safety against a hostile host.

Apply checks the conversion input inventory's exact digests and modes again before writes and checks each edited file at its write boundary. An exclusive `.acr-producer-transaction` directory holds synced before-images and staged output. Creation is exclusive; detected races refuse. A failed apply restores the original files and modes. Rollback failures explicitly name the affected paths and retain backups. An interrupted transaction blocks a new conversion until its backups have been inspected and recovery completed; the command never silently discards an interrupted claim.

The input inventory covers the selected authored tree (excluding consumer configuration), `.github` delivery files, root output paths, competing producer markers and license/notice files at the package or its ancestors. Outside the selected tree, producer discovery reads accessible directory entries without opening unrelated files. Unreadable unrelated directories are outside this discovery scope. Other repository documents, secrets and installed consumer state are neither read into the plan nor fingerprinted. Changes to unrelated files do not invalidate a plan or rerun. In both deterministic and semantic clean conversion, known consumer surfaces such as `.github/mcp.json`, `.gemini/settings.json`, `.vscode/settings.json` and `.openhands/config.toml` are excluded before file reads, fingerprinting or planning, at the checkout root, whether the authored package is at that root or nested. They remain opaque even when they contain Tessl settings or are symlinks. Adding, removing or editing only those settings cannot invalidate Apply or an inert rerun. Similarly named support files elsewhere in an authored skill remain producer input.

A versioned `.acr-producer-migration.json` receipt is initially written with mode `0600` and is excluded from publication. Commit it with the converted checkout. Schema version 2 records source identity/version, normalized conversion options, relevant file digests and executable status, artifacts and distribution inventory. `internal/producerconvert` is its sole writer/reader. Empty directories and non-executable permission differences are excluded from receipt comparison, so a normal Git commit/clone, including a receipt checked out at `0644`, supports an inert rerun after source manifests are gone: exit 0, `current: true`, `wrote: false`. Relevant file additions/removals, changed bytes or executable status, changed options, malformed receipts and unknown receipt versions refuse. Schema version 1 receipts from the unreleased initial implementation require starting again from the original source checkout; they are not silently reinterpreted. To perform a different conversion, also begin from the original source checkout. Preview and unsupported input create no receipt or temporary files.

### Custom-code limitation

This initial deterministic implementation does **not** complete issue #117's untouched-Good-OSS-Citizen acceptance. Custom operations that read/write Tessl configuration, declare or resolve dependencies, build installed paths dynamically, or change Tessl-backed review policy return `unsupported_semantic_conversion` with actionable paths before writes. The converter detects literal Tessl operations; it cannot infer arbitrary program behavior or port obfuscated/dynamically synthesized code. Historical Tessl mentions and research URLs are not by themselves executable dependencies.

With `--agent`, the [semantic conversion contract](cli.md#semantic-producer-conversion) also applies: credential filenames in the selected package and shared workflow/test input scopes refuse before their contents are read or sent. `.env.example` and `.env.sample` must contain placeholders. Reports retain request digests and bounded provider diagnostics, without raw requests; ordinary source and provider output still require appropriate confidentiality.

Explicit `.tessl-plugin` directory components in runtime/support files, templates, tests, executable files and hook arguments require semantic conversion even when `plugin.json` is joined separately or paths use Windows separators. Markdown instructions receive the same check on the code a reader copies and runs: fenced code blocks, indented code blocks and inline code spans holding a command with arguments. Surrounding prose, links, single-name spans such as `.tessl-plugin` and recognized legal notices retain their existing literal-operation checks; a historical directory mention alone does not widen that scope. Code inside block quotes or raw HTML is not classified. Proposed output is checked by the same planner before mutation. Select `--agent codex` or `--agent claude` to request the existing semantic conversion; without a selected agent these dependencies refuse before writes.

GOC's original install-gate reads its installed plugin version, writes dependency state while preserving pins, installs templates and coordinates rollback. Its preflight/commit helpers and review workflows also depend on Tessl state and policy. Substituting path strings cannot preserve those semantics. A reusable semantic migration and the replacement review policy still require a product decision and positive end-to-end tests. A GOC refusal proves this boundary, not successful GOC migration.

`cmd/acr/producer_clean_test.go` runs clean migration through the shipped binary on an independent nested package, then uses the existing HTTP subprocess journey to publish its actual assets, install, realize and check all three adapters, and directly execute the materialized cross-skill helper with Tessl absent.

## Inputs and output

This section and the mapping, path-preservation and republication sections below describe default dual-distribution conversion.

The converter reads `tile.json` and `.tessl-plugin/plugin.json`. It writes exactly one file, `agent-plugin.yaml`. `--dry-run` writes nothing.

When both Tessl manifests are present, `plugin.json` is authoritative. A scalar field only one manifest declares is used; a scalar both declare differently is `ambiguous_manifest`. A `rules` or `skills` set declared on only one side is also `ambiguous_manifest`, and directory forms are expanded before comparison. tile.json silence on hooks is not disagreement: tile.json cannot declare hooks.

The converted document is a v1 `agent-plugin.yaml` as validated by `internal/manifest`. Artifact IDs come from the tile.json key when tile.json declares that path; otherwise they are the path basename. Two hooks that share a basename at different events both become `<basename>-<event>`. Any other ID collision is `duplicate_artifact_id` from self-validation.

`source.repository` must equal `https://github.com/<name>`. An omitted Tessl `repository` is filled from `--repository`. The converter never synthesizes the URL from `name`.

## Mapping

| Tessl | agent-plugin.yaml | Notes |
| --- | --- | --- |
| `name`, `version` | `name`, `version` | Verbatim |
| `description` / tile `summary` | `description` | Verbatim |
| `repository` | `source.repository` | Must match `https://github.com/<name>` |
| `name` | `source.tesslIdentity` | Recorded so the package's own `.tessl/plugins/<identity>/...` references keep resolving |
| `homepage`, `license`, `author` | — | Lossy provenance; conversion still writes |
| `private: false` | — | No-op, not reported |
| `private: true` | — | Unmapped, blocking |
| `rules` / `skills` | `artifacts.rules` / `artifacts.skills` | Paths preserved; directory form expands; tile `SKILL.md` paths become skill directories |
| — | `artifacts.scripts` | Always empty; Tessl siblings stay inside the skill |
| `hooks` / `nativeHooks` | `artifacts.hooks` | Event de-spelled onto the v1 vocabulary; `${TESSL_PLUGIN_DIR}/` stripped |
| `hooks[].matcher` | — | Unmapped, blocking |
| `.tesslignore` / `.tileignore` | — | Reported verbatim, never interpreted or copied |
| unknown keys | — | `unknown_field`, blocking |

Rule activation is read from the source file frontmatter, not from the Tessl manifest. `alwaysApply: true` becomes `always`. `alwaysApply: false` plus an em-dash-separated glob half of `applyTo:` / `globs:` / `paths:` becomes `paths`. Missing frontmatter, a missing em dash, or `false` with no globs is `invalid_rule_activation`.

## Republishing a package whose references stopped resolving

A package whose files address their own helpers through `.tessl/plugins/<workspace>/<package>/...` needs `source.tesslIdentity` for those references to resolve once ACR owns the tree. Conversion records it from the Tessl name it converted. A package published before the field existed carries none, and its legacy references are preserved unrewritten rather than pointed at a tree ACR cannot prove it owns.

The following manual identity/version steps apply only to default dual-distribution conversion. Clean mode supplies both in the command above. Restoring one means publishing a new version. Two identities are involved and only one of them is derivable: `source.repository` must equal `https://github.com/` + `name`, so conversion cannot be handed the publication repository — `acr migrate tessl-plugin --repository https://github.com/<new-owner>/<new-repo>` against a plugin still named `<workspace>/<package>` exits 1 with `invalid_source`. Conversion runs under the original identity, and the producer authors the publication identity afterwards, in the manifest it has not published yet.

1. Delete the existing `agent-plugin.yaml`. The converter refuses to overwrite differing bytes with `manifest_conflict`.
2. Convert under the **original** Tessl identity, with `.tessl-plugin/plugin.json` still naming it:

   ```shell
   acr migrate tessl-plugin --repository https://github.com/<workspace>/<package>
   ```

3. Confirm the written manifest carries `source.tesslIdentity: <workspace>/<package>`.
4. Only when the package is published from a repository whose name differs from the Tessl identity: edit exactly two fields of the written `agent-plugin.yaml`. Set `name` to the publication package identity and `source.repository` to `https://github.com/<that identity>`. Leave `source.tesslIdentity` alone — it records the identity the package's own files address, and it is independent of where the package is hosted. A package published from a repository of the same name skips this step and only this step.
5. Set `version` to the new release version. Conversion rewrites `version` from the Tessl manifest, so a republication always regenerates the version already published; leaving it puts the previous tag back in the manifest, and `git tag` then fails with `tag 'v<version>' already exists`. Every republication reaches this step.
6. Commit, tag and `acr publish`.

`cmd/acr/producer_rename_test.go` runs both branches through the production commands against a local fake remote. `TestRenamedProducerPublishRoundtrip` covers the renamed branch, including the `invalid_source` refusal in step 2. `TestSameNameRepublishRoundtrip` publishes an old version first, then republishes over it: the earlier tag and release survive, the new version installs, and the commands the realized skills and rules instruct an agent to run all execute.

Consumers on an ACR release older than `source.tesslIdentity` reject the field, because `manifest.Load` pins `schemaVersion: 1` and refuses unknown keys. Ship the CLI release before the package.

## Path preservation

The package has one writer: `os.OpenRoot`, a `Root.OpenFile` of package-local `.agent-plugin.yaml.tmp` with `O_WRONLY|O_CREATE|O_EXCL`, then `Root.Rename` onto `agent-plugin.yaml` after a successful sync and close. A leftover temp from a crashed run is removed before the next write. Artifact source files are never created, truncated, renamed, chmodded, or removed.

Emitted paths match the Tessl paths except two reversible normalizations: a trailing `/SKILL.md` is stripped because a v1 skill is a directory, and a `${TESSL_PLUGIN_DIR}/` prefix is stripped because a v1 path is package-relative. A backslash or absolute rule or skill path is `invalid_path`; a hook command outside the closed `${TESSL_PLUGIN_DIR}/` grammar is `unmapped_field`.

## Idempotency and dual manifests

A second run that would write the same bytes exits 0 with `wrote: false`. Differing bytes are `manifest_conflict`; the tool never overwrites a hand edit. Delete `agent-plugin.yaml` and re-run to replace it.

`plugin.json`, `tile.json`, `tessl-package.json`, `README.md`, and ignore files stay on disk. `manifest.PackageFiles` is driven only by `agent-plugin.yaml`, so those Tessl files are not published. `manifest.Load` reads only `agent-plugin.yaml`. To retire supported Tessl producer metadata, use the explicit `--acr-only` mode described above. Default conversion never performs that cleanup.

## Report and exit codes

Both modes use exit `0` for a complete/current result, `1` for refusal and `2` for usage. `--package-version` outside clean mode, `--acr-only` without explicit `--repository`, or either new flag on another command is a usage error. Clean reports expose `changes`, `blockers`, `publishedFiles`, old/new identity and receipt path.

For default conversion, `--json` success writes one envelope to stdout. Failures write one error envelope to stderr; when conversion identifies unmapped input, its partial report is included as `result` with the populated `unmapped` entries. Text failures print the same unmapped entries after the diagnostic. Exit `0` means written or already current, `1` is a named refusal, and `2` is usage. Conversion never uses exit `3` or `4`.

Blocking refusals (`unmapped`, no write): `private: true`, `matcher`, an event outside v1, a command outside the closed hook grammar, diverging `nativeHooks` bodies, `unknown_field`, and `agent_widening`.

Lossy (exit 0, written): `author`, `license`, `homepage`, rule `description:`, the `applyTo:` glob and prose halves, and a tile key that differs from its basename.

Ignore-file lines and `tessl-package.json` are informational. The report's `publishedFiles` equals `manifest.PackageFiles`. A published path carrying `__pycache__`, `node_modules`, `.git`, `.DS_Store`, or a `.pyc`/`.pyo` file is `unpublishable_content`.

`nativeHooks` that omit any ACR adapter are `agent_widening`, because a converted hook would fire on agents Tessl never configured. Move the entry into consensus `hooks`, or re-run with `--accept-agent-widening` to accept that one class.

The converter runs `manifest.Validate` on its output before writing and surfaces #4 codes such as `invalid_source`, `required`, `no_artifacts`, `invalid_rule_activation`, `invalid_path`, and `duplicate_artifact_id` verbatim.

### Paid scoring declarations

The exact selected `plugin.json` and `tile.json` are mapped protected metadata: descriptive text does not require a paid-policy declaration, and agents still cannot edit them. For each other changed file originally containing `skill-review` or `score`/`threshold` followed by `85` on the same line, semantic conversion requires a `policyChanges` record for that path. Its `from` must identify the paid Tessl service and its `to` must declare retirement. Every retained affected text file must visibly state retirement and the lack of an equivalent score, after semantic edits and all deterministic transformations. A record alone or a notice alone is insufficient.

The declaration syntax is finite. Matching ignores case, folds whitespace, and treats ASCII hyphens as spaces. These are the supported phrases after normalization:

| Part | Accepted form |
| --- | --- |
| Service identity | `paid tessl` or `tessl paid`, followed by `skill review`, `changed skill review`, `threshold 85 skill review`, `score` or `score gate` |
| `from` | Starts with the service identity, followed by whitespace, a period, semicolon, colon or the end of the field |
| `to` | Starts with `retired`, `removed`, `retire tessl skill review`, `retire paid tessl skill review`, `retire score`, `retire paid score`, `retire score gate`, `retire paid score gate`, `remove the scoring only workflow` or `visibly disclose removal of the paid score gate`, with the same boundary as `from` |
| Visible retirement | A service identity, optionally followed by `threshold 85 workflow`, `workflow` or `gate`, then `was retired`, `is retired`, `has been removed` or `was removed` |
| No equivalent score | Contains `ACR has no equivalent score` or `without an equivalent score gate` |

A concise retained notice is: **Paid Tessl skill review was retired; ACR has no equivalent score.** For example, use `from: Paid Tessl skill review` and `to: Retired; ACR has no equivalent score.` The longer declaration “the paid Tessl skill-review threshold-85 workflow has been removed without an equivalent score gate. ACR validation is not a review score” is also supported. Metadata may use different supported wording from visible text.

A proven service-only workflow may be deleted entirely. Deleted files and the opaque `.github/aw/actions-lock.json` supply the no-equivalent phrase in `to`; do not add comments to JSON or create an unrelated notice file. Retained workflow and skill text still needs its own visible declaration. These checks recognize the stated syntax, not arbitrary prose equivalence, and do not prove that independent tests or reviews retain their meaning. Those obligations remain subject to their separate validation and output review.
