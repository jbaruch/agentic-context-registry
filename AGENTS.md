# Agent Contribution Requirements

If you don't have have ownership on this repository, before making a substantive change in this repository:

1. Install Tessl if the `tessl` command is unavailable.
2. Run `tessl install tessl-labs/good-oss-citizen` from the repository root.
3. Read and follow `CONTRIBUTING.md`.
4. Preserve existing project and agent instructions when realizing generated context.

## Agent Rules <!-- tessl-managed -->

@.tessl/RULES.md follow the [instructions](.tessl/RULES.md)

## Author or audit an ACR plugin

When asked to create an ACR plugin from an existing repository, or assess whether
that repository is compliant, read [Plugin authoring](docs/plugin-authoring.md).
Use the [manifest contract](docs/package-manifest.md),
[schema](schemas/agent-plugin.schema.json), and
[examples](examples/complete/agent-plugin.yaml) as the source requirements.

- Audit read-only by default; create or edit files only when requested. Preserve
  existing instructions, source content, and application structure.
- Identify the package root and check `agent-plugin.yaml`, identity, version,
  declared paths, artifact shapes, activation policies, and hook events. Folder
  names such as `rules/` and `skills/` are conventions, not validity requirements.
- Check complete skill support files, references, and distribution inventory.
  Separate authored package content from consumer `agents.yaml`, lockfiles, and
  generated native output.
- Distinguish local validity, GitHub publish/install readiness, and adapter/runtime
  verification. A nested manifest alone does not enable subdirectory installation.
  Check CLI availability before using `acr validate` (pending v0.2.0; absent in v0.1.6).
- Return the guide's [compliance report](docs/plugin-authoring.md#compliance-report)
  with passed, failed, and not-checked criteria. Every failure needs a concrete
  path or manifest field, the reason, and the smallest suggested fix.
