# Author a plugin from an existing repository

An ACR plugin is a package of rules, skills, scripts, or hooks described by
`agent-plugin.yaml`. You can add that manifest to an existing application
repository and reference its existing authored content. Application code does
not need to move, and conventional folders do not need to be created when the
content already lives elsewhere.

For a compliance request, inspect and report **read-only by default**. Do not
rewrite files, install dependencies into the target, initialize consumer state,
create tags, or publish as part of an audit. When creation or correction is
explicitly requested, follow the workflow below and make only the needed edits.
Preserve existing agent instructions and managed blocks.

The authoritative contracts are the [manifest specification](package-manifest.md),
[JSON Schema](../schemas/agent-plugin.schema.json), and executable
[minimal](../examples/minimal/agent-plugin.yaml) and
[complete](../examples/complete/agent-plugin.yaml) examples. This guide explains
how to apply them; it adds no new CLI behavior.

## Choose the package root

The **package root** is the directory containing `agent-plugin.yaml`. Every
artifact path is relative to that directory, regardless of the shell's current
directory. Keep these three surfaces separate:

| Surface | Files | Purpose |
| --- | --- | --- |
| Authored package | `agent-plugin.yaml` and its declared content | What the producer distributes |
| Consumer project | `agents.yaml` and `.agents/registry.lock` | Which packages and agents a project uses, with immutable resolutions |
| Generated native output | ACR-owned files/entries under `.claude/`, `.codex/`, `.cursor/`, shared instruction hosts, and optional `.agents/skills/` | Adapter realization in the consuming project |

`acr init` initializes consumer selections; it does not scaffold a plugin or
create `agent-plugin.yaml`. Existing native configuration is not an ACR source
manifest. Inventory authored content separately from generated copies, even
when the same repository is both a producer and a consumer. See
[dependencies](dependencies.md) and [realization](realization.md).

For installation as `github:owner/repository`, put `agent-plugin.yaml` at the
**Git repository root**. It can point into existing subdirectories, such as
`docs/agent-guidance.md` or `automation/review`. The package identity must be
`owner/repository`, matching its actual GitHub source.

A nested package, for example `plugins/review/agent-plugin.yaml`, can be checked
with `acr validate plugins/review`. The publisher also accepts a local package
`PATH` and reads declared files relative to it from the enclosing tagged Git
tree. That local path selection does **not** make the nested package installable
from the enclosing repository:

- `acr install github:owner/repository[@TAG|@SHA]` downloads the GitHub repository
  source archive and loads the manifest at its root. It does not search nested
  directories or install the publisher's filtered release archive.
- There is no source subdirectory selector, nested install flag, or generic local
  directory install command. `--project` selects a consumer project, not a remote
  package subdirectory.
- To distribute content kept in a subdirectory, use one root manifest with paths
  into that content. For an independently versioned plugin, arrange a separate
  GitHub repository with the package at its root and matching identity. Exporting
  or splitting such a repository is a separate workflow, not an ACR install feature.

Report a valid nested package as locally valid with GitHub installation not ready
when the enclosing repository has no root manifest. If a root manifest exists,
installation uses that package, not the nested one.

## Existing-repository workflow

1. **Inventory without changes.** Record the intended package root, GitHub
   repository, current manifest if any, and candidate authored content. Identify
   rules, skill entrypoints and their support trees, standalone helpers, and
   lifecycle hooks. Record licenses/notices and any external runtime dependencies.
   Do not select generated native copies as the source of truth.
2. **Map content to artifacts.** Give each artifact a stable unique ID, its actual
   package-relative path, and the metadata below. A skill is its complete directory;
   a rule, standalone script, or hook is one file. Leave application code and
   valid custom folder names in place.
3. **Describe the package.** For an audit, report a missing or invalid manifest
   with the smallest fix. For requested creation/editing, add or update
   `agent-plugin.yaml` using the minimal example below and the full contract.
   Add only missing content or metadata; preserve existing instructions. An
   existing Tessl producer has a separate [conversion workflow](migration-producer.md).
4. **Check local validity and completeness.** Run the available local validator,
   inspect the returned distribution inventory, and review references and runtime
   requirements. Record each failure's field/path and a proposed fix. Recheck
   after authorized edits; an unavailable check is `NOT CHECKED`, never a pass.
5. **Assess delivery separately.** Record whether the package is at the GitHub
   archive root, whether files/modes are committed, and whether the tagged release
   prerequisites and adapter checks have been demonstrated. Only when release
   preparation is requested, follow [publishing](publishing.md); an audit ends
   with the [compliance report](#compliance-report).

## Minimal package and custom layouts

This is a complete minimal manifest at `agent-plugin.yaml`. Replace the example
identity and repository together with your own before distribution. The example
GitHub URL is illustrative; local validation does not check that it exists.

```yaml
schemaVersion: 1
name: example/project-tools
version: 1.0.0
source:
  repository: https://github.com/example/project-tools
artifacts:
  rules:
    - id: project-guidance
      path: guidance/project.md
      activation:
        mode: always
```

The corresponding layout is:

```text
existing-repository/
  agent-plugin.yaml
  guidance/
    project.md
  src/                  # existing application code stays here
```

`guidance/project.md` can contain:

```markdown
# Project Guidance

Run the project's documented tests before submitting a change.
```

There is no requirement to rename `guidance/` to `rules/`. The familiar
`rules/`, `skills/<name>/`, `scripts/`, and `hooks/` layout is a convention. The
manifest-declared path and the artifact's source shape determine validity.

For example, this alternative complete manifest declares a custom skill layout:

```yaml
schemaVersion: 1
name: example/project-tools
version: 1.0.0
source:
  repository: https://github.com/example/project-tools
artifacts:
  skills:
    - id: review-change
      path: automation/review
```

```text
automation/review/
  SKILL.md
  references/review-guide.md
  scripts/check.sh
```

All three files ship as one skill. The path is `automation/review`, not
`automation/review/SKILL.md` and not the parent `automation` unless that parent
itself has the intended `SKILL.md`.

## Source compliance checklist

### Manifest and metadata

- Require one regular `agent-plugin.yaml` at the selected root, with one YAML
  document and only fields allowed by schema version 1.
- Require integer `schemaVersion: 1`, lowercase `name: owner/repository`, a valid
  semantic `version` such as `1.0.0` (no leading `v`), `source.repository`, and
  `artifacts` containing at least one rule, skill, script, or hook.
- Require `source.repository` to equal `https://github.com/` plus `name` exactly;
  no `.git` suffix, trailing slash, branch URL, or subdirectory URL. Full naming
  and version patterns are in the schema. Local validation checks this identity
  relationship, not repository existence or access.
- `description` is optional. `source.tesslIdentity` is optional migration metadata;
  omit it in a new plugin. If migration supplied it, preserve the valid original
  identity. Do not add unsupported fields such as `license`, `dependencies`, or
  agent-native configuration to the v1 manifest; document those needs in content.
- IDs are lowercase kebab case, start with a letter, and are unique across all
  artifact classes. Preserve IDs when moving source paths.

### Shapes, activation, paths, and modes

| Class | Required source shape | Required metadata beyond `id` and `path` |
| --- | --- | --- |
| Rule | Regular file, normally Markdown prose | `activation.mode: always` with no path filters, or `mode: paths` with one or more unique relative POSIX globs in `activation.paths` |
| Skill | Directory containing a regular, case-sensitive `SKILL.md` | None in the manifest; include useful skill instructions and discovery metadata in the file |
| Script | Regular file | None |
| Hook | Regular entrypoint file | Canonical `event`; optional `args` array of strings |

Hook events are `session-start`, `session-end`, `user-prompt-submit`,
`pre-tool-use`, `post-tool-use`, and `stop`. Native names such as `SessionStart`
do not belong in this manifest. Adapters select native event/configuration forms.

Artifact paths must be normalized relative POSIX paths. Use `guidance/project.md`,
not `/guidance/project.md`, `./guidance/project.md`, `guidance//project.md`,
`guidance/../project.md`, a trailing slash, a Windows drive path, or backslashes.
The path and every component below the package root must exist without symbolic
links. Declared files must be regular; skill trees may contain only directories
and regular files, with no links or special files.

Local structural validation does not require every source file to have mode
`0755`. Publication reads committed Git modes: `100644` becomes `0644`, and
`100755` becomes `0755`. Record the executable bit for skill helpers that are
invoked directly, and supply a working interpreter/shebang where needed. Adapters
render standalone scripts and hooks as executable; executable skill support files
retain their executable mode. Neither a mode bit nor validation proves that a
script runs correctly. See [adapter behavior](adapters.md#native-adapters).

### Skill support and distribution

Write a useful `SKILL.md` with `name` and a trigger-oriented `description` in
frontmatter, then instructions appropriate to its intended agent. The local
manifest validator checks the file's presence and regular-file shape; it does not
review its prose, enforce all native skill metadata, or run its commands.

A published package archive includes exactly the manifest, declared rule/script/
hook files, and all regular files recursively inside declared skill directories.
Files are sorted and duplicate references appear once. Undeclared files outside
skill trees do not enter that archive: application code, root README/license
files, CI files, and other tool manifests are not automatically included.
`.gitignore` and `.tesslignore` are not ACR distribution filters. A file inside a
declared skill tree is selected even if ignored by Git; publication still needs
it committed. Review that inventory for accidental credentials, caches, or
unrelated content. The GitHub source archive used for installation remains the
repository archive, so filtered release assets are not a repository privacy filter.

Keep each skill's helpers, references, assets, and required license/notice copies
in its distribution tree. Preserve original attribution files. Check every local
reference against the files that will actually ship; a link to an undeclared
root-level document may exist in the checkout and fail in the realized skill.
For commands intended to run from the consumer project root, use the declared
package-root skill path, for example `sh automation/review/scripts/check.sh`.
Adapters rebase supported references to declared skill trees, including custom
paths; they do not rewrite arbitrary application paths, external URLs, or every
possible shell expression. See the [reference boundary](adapters.md#native-adapters).
Audit dependencies on consumer files, interpreters, tools, services, and working
directories explicitly; structural validation does not establish self-containment
or semantic equivalence.

## Commands and what they prove

`acr validate` is new in the source intended for **v0.2.0 (pending)** and is absent
from released **v0.1.6**. Check `acr version --json` and `acr help validate` on the
binary you actually use. A development build may report a commit-based pseudo
version instead of `v0.2.0`. If the command is unavailable, use a build containing
it as described in [installation](install.md), or perform the checklist manually
and mark CLI validation `NOT CHECKED`. Do not substitute consumer initialization
or publication for an offline structural check.

The following commands are templates; replace `PATH` with the package root:

```shell non-executable
acr version --json
acr help validate
acr validate PATH
acr validate PATH --json
```

`PATH` defaults to `.`. A relative validation path resolves against `--project`;
with neither argument it uses the working directory. For example,
`acr validate nested --project ../producer` checks `../producer/nested`.
Validation performs no Git lookup, credential lookup, network access, or writes.
It exits `0` on success, `1` for invalid input/operational failure, and `2` for
invalid CLI usage. It has no `--dry-run` option because it is already read-only.
JSON success contains `result.valid`, `result.name`, `result.version`, and the
sorted `result.files`. Errors go to stderr with `error.code`, `error.message`,
and a manifest `error.field` when available; a missing manifest has no field.

This executable example uses the documentation harness's small publisher fixture:

```console
$ acr validate --json
# fixture: publisher
# exit: 0
{"ok":true,"command":"validate","result":{"valid":true,"name":"example/alpha","version":"1.0.0","files":["agent-plugin.yaml","guidance.md"]}}
```

Schema validation covers document shape. `acr validate` adds filesystem,
cross-artifact, and distribution inventory checks. Neither establishes semantic
quality, actual native agent behavior, or successful publication.

When publication preparation is requested, use the separate rehearsal:

```shell non-executable
acr publish PATH --dry-run --json
```

It requires a clean Git worktree, all selected files committed, exactly one tag
at `HEAD`, and a tag matching the manifest version with at most one leading `v`.
It builds from tagged blobs, runs apply and idempotent check realization through
Claude Code, Codex, and Cursor in fresh projects, and checks that GitHub has the
same tag at the same commit with no conflicting visible release. It needs Git,
network access and applicable credentials; it writes private temporary files,
but performs no GitHub writes. It is not an untagged local lint command.
Only an explicitly requested publication should omit `--dry-run`.

A passing publisher adapter gate proves generated layout and idempotence, not
that real agents execute the skill or hook correctly. Report actual runtime
checks separately, using [manual conformance](manual-conformance.md) and the
package's own deterministic tests. `acr check` checks a consumer's native-layout
drift; it does not validate an arbitrary authored plugin directory.

For installation readiness, verify the root manifest in the referenced GitHub
commit. A commit pin can install without a GitHub Release; an unversioned request
requires a latest stable release. ACR release metadata is additive evidence, not
a prerequisite for every existing package. See [publishing](publishing.md) and
[installation policies](dependencies.md).

## Diagnose concrete failures

These are example findings, not a claim that every check runs in `acr validate`.
Field indexes are zero-based. Always include the selected package root in a report.

| Location | Reason / observed code | Smallest suggested fix |
| --- | --- | --- |
| `agent-plugin.yaml` | Missing manifest; CLI `operation_failed` names the missing file | Add the manifest at the intended package root, or select the existing correct root |
| `agent-plugin.yaml:artifacts.rules[0].path` = `rules/project.md` | `path_not_found`; the file actually lives at `guidance/project.md` | Point the declaration to `guidance/project.md`; no folder rename needed |
| `agent-plugin.yaml:artifacts.rules[0].path` = `guidance` | `invalid_artifact_type`; a rule points to a directory | Point to the intended regular file `guidance/project.md` |
| `agent-plugin.yaml:artifacts.skills[0].path` = `automation/review/SKILL.md` | `invalid_artifact_type`; a skill points to a file | Declare `automation/review` |
| `automation/review/SKILL.md` | `path_not_found` at `artifacts.skills[0].path`; the declared directory has no entrypoint | Add the intended `SKILL.md`, correct its case, or select the directory that already contains it |
| `automation/review/references/guide.md` | `invalid_skill_tree` when this is a symbolic link | Replace the link with the needed regular file inside the skill tree |
| `agent-plugin.yaml:source.repository` | `invalid_source`; URL disagrees with `name` | Set both fields to the actual canonical GitHub identity |
| `agent-plugin.yaml:artifacts.hooks[0].event` = `SessionStart` | `unsupported_hook_event` | Use `session-start` |
| `automation/review/SKILL.md` links to undeclared `docs/checklist.md` | Distribution/reference audit failure; local validation can still pass | Bundle the needed document in the skill tree and update the reference |
| `plugins/review/agent-plugin.yaml` with no root manifest | Local validation may pass; repository installation cannot find a root package | Add a root manifest referencing the content, or distribute a separate repository with matching root identity |
| `agent-plugin.yaml:version` versus the tag at `HEAD` | Publisher `tag_version_mismatch` | Prepare a new matching version/tag under the release workflow; do not move a published tag |

A valid custom path such as `guidance/project.md` or `automation/review` is a
**PASS**. Absence of folders named `rules/`, `skills/`, `scripts/`, or `hooks/`
is not itself a finding.

## Compliance report

Use this format after a read-only audit, filling every row with evidence. Use
`PASS`, `FAIL`, or `NOT CHECKED`; explain checks you could not run. Report source
validity separately from delivery readiness, without turning an unchecked item
into a pass. Include CLI version/source commit, commands, exit codes, and the
manifest field or filesystem path for each failure.

```text
Repository: <URL or local path>
Package root: <absolute path>; Git root: <absolute path or not present>
Mode: read-only audit | requested creation/edit
ACR version/commit: <observed value or unavailable>
Source validity: PASS | FAIL | NOT CHECKED
GitHub installation readiness: PASS | FAIL | NOT CHECKED
Publication readiness: PASS | FAIL | NOT CHECKED

Criterion                         Status        Evidence
Manifest and required metadata    <status>      <file/fields/check result>
Declared paths and source shapes  <status>      <paths and artifact IDs>
Activation, IDs, hook events      <status>      <fields/check result>
Skill support and local references <status>     <selected files/reference checks>
Distribution files and Git modes  <status>      <inventory and committed modes>
GitHub archive root and identity  <status>      <repository/ref/root evidence>
Tag, clean tree, remote release   <status>      <ref/commit/dry-run result>
All-adapter layout/idempotence    <status>      <publisher gate result or not run>
Real agent/runtime behavior       <status>      <agent versions/tests or not run>

Findings:
- [FAIL] <path or manifest field>: <reason and code if observed>.
  Smallest fix: <specific edit; do not apply during a read-only audit>.
- [PASS] <valid nonstandard layout and supporting evidence>.
- [NOT CHECKED] <criterion>: <missing tool, access, tag, or runtime evidence>.

Commands and results: <exact commands, exit codes, relevant output>
Changes made: none | <explicitly requested changes>
Next action: <smallest step needed for the user's requested outcome>
```
