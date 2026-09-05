---
name: advocate
description: Fixture skill whose helper is addressed through both supported reference forms when assessing reference rebasing.
---

# Advocate

Process steps in order. Do not skip ahead.

## Step 1 — Run the helper through its package-root path

Run `skills/advocate/scripts/check.sh --info`.

## Step 2 — Run the helper through its legacy Tessl-installed path

Run `.tessl/plugins/legacy-workspace/advocate-plugin/skills/advocate/scripts/check.sh --info`.

## Step 3 — Leave every unsupported reference alone

- Remote URL: `https://example.com/skills/advocate/scripts/check.sh`
- Interior segment: `vendor/skills/advocate/scripts/check.sh`
- Longer directory name: `myskills/advocate/scripts/check.sh`
- Sibling directory name: `skills/advocate-archive/scripts/check.sh`
- Another package's skill: `.tessl/plugins/other-workspace/other-plugin/skills/unrelated/check.sh`
- Tessl path missing the package segment: `.tessl/plugins/legacy-workspace/skills/advocate/scripts/check.sh`
- Skill root without a trailing separator: `skills/advocate`

Finish here.
