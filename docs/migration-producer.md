# Producer conversion from Tessl plugin manifests

`acr migrate tessl-plugin [PATH]` converts a Tessl plugin package into `agent-plugin.yaml` without rewriting rule, skill, script, or hook source files. PATH is the plugin package root and defaults to `.`, the same positional argument `acr publish [PATH]` uses.

```text non-executable
acr migrate tessl-plugin [PATH] [--dry-run] [--json] [--repository URL] [--accept-agent-widening]
```

Consumer inventory (`acr migrate tessl`) is a separate command. This page covers only producer conversion.

For the release sequence after conversion, including how the Tessl and ACR manifests share one version tag, follow the [dual-publishing contract](publishing.md#dual-publishing).

## Inputs and output

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

A package whose files address their own helpers through `.tessl/plugins/<workspace>/<package>/...` needs `source.tesslIdentity` for those references to resolve once ACR owns the tree. Conversion records it; a package published before it existed carries none, and its legacy references are preserved unrewritten rather than pointed at a tree ACR cannot prove it owns.

To restore one:

1. Convert the original producer again — `acr migrate tessl-plugin --repository https://github.com/<owner>/<repo>` — with `.tessl-plugin/plugin.json` still naming the Tessl identity. Delete the existing `agent-plugin.yaml` first; the converter refuses to overwrite differing bytes.
2. Confirm the written manifest carries `source.tesslIdentity`.
3. Publish a new release. A package hosted under a repository whose name differs from the Tessl identity keeps `name` and `source.repository` bound to that repository; `source.tesslIdentity` records the identity the files address, and the two are independent.

Consumers on an ACR release older than `source.tesslIdentity` reject the field, because `manifest.Load` pins `schemaVersion: 1` and refuses unknown keys. Ship the CLI release before the package.

## Path preservation

The package has one writer: `os.OpenRoot`, a `Root.OpenFile` of package-local `.agent-plugin.yaml.tmp` with `O_WRONLY|O_CREATE|O_EXCL`, then `Root.Rename` onto `agent-plugin.yaml` after a successful sync and close. A leftover temp from a crashed run is removed before the next write. Artifact source files are never created, truncated, renamed, chmodded, or removed.

Emitted paths match the Tessl paths except two reversible normalizations: a trailing `/SKILL.md` is stripped because a v1 skill is a directory, and a `${TESSL_PLUGIN_DIR}/` prefix is stripped because a v1 path is package-relative. A backslash or absolute rule or skill path is `invalid_path`; a hook command outside the closed `${TESSL_PLUGIN_DIR}/` grammar is `unmapped_field`.

## Idempotency and dual manifests

A second run that would write the same bytes exits 0 with `wrote: false`. Differing bytes are `manifest_conflict`; the tool never overwrites a hand edit. Delete `agent-plugin.yaml` and re-run to replace it.

`plugin.json`, `tile.json`, `tessl-package.json`, `README.md`, and ignore files stay on disk. `manifest.PackageFiles` is driven only by `agent-plugin.yaml`, so those Tessl files are not published. `manifest.Load` reads only `agent-plugin.yaml`. The converter does not offer `--remove-tessl`; retiring Tessl distribution is a later release decision.

## Report and exit codes

`--json` success writes one envelope to stdout. Failures write one error envelope to stderr; when conversion identifies unmapped input, its partial report is included as `result` with the populated `unmapped` entries. Text failures print the same unmapped entries after the diagnostic. Exit `0` means written or already current, `1` is a named refusal, and `2` is usage. Conversion never uses exit `3` or `4`.

Blocking refusals (`unmapped`, no write): `private: true`, `matcher`, an event outside v1, a command outside the closed hook grammar, diverging `nativeHooks` bodies, `unknown_field`, and `agent_widening`.

Lossy (exit 0, written): `author`, `license`, `homepage`, rule `description:`, the `applyTo:` glob and prose halves, and a tile key that differs from its basename.

Ignore-file lines and `tessl-package.json` are informational. The report's `publishedFiles` equals `manifest.PackageFiles`. A published path carrying `__pycache__`, `node_modules`, `.git`, `.DS_Store`, or a `.pyc`/`.pyo` file is `unpublishable_content`.

`nativeHooks` that omit any ACR adapter are `agent_widening`, because a converted hook would fire on agents Tessl never configured. Move the entry into consensus `hooks`, or re-run with `--accept-agent-widening` to accept that one class.

The converter runs `manifest.Validate` on its output before writing and surfaces #4 codes such as `invalid_source`, `required`, `no_artifacts`, `invalid_rule_activation`, `invalid_path`, and `duplicate_artifact_id` verbatim.
