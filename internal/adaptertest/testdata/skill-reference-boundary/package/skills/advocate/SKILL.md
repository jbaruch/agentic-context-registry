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

## Step 3 — Read the companion through a Markdown destination

See [the helper](skills/advocate/scripts/check.sh) and the parenthesized
form (skills/advocate/scripts/check.sh) and the escaped form
\.tessl/plugins/legacy-workspace/advocate-plugin/skills/advocate/scripts/check.sh
and the option assignment --helper=skills/advocate/scripts/check.sh
and the environment assignment HELPER=skills/advocate/scripts/check.sh
and the quoted argument "skills/advocate/scripts/check.sh"
and the backquoted command `python3 skills/advocate/scripts/check.sh --quoted`
and the quoted option assignment --file="skills/advocate/scripts/check.sh"
and the quoted environment assignment QUOTED_HELPER="skills/advocate/scripts/check.sh"
and the single-quoted environment assignment SINGLE_HELPER='skills/advocate/scripts/check.sh'
and the Markdown destination whose [label wraps
onto a second line](skills/advocate/scripts/check.sh)

## Step 4 — Leave every unsupported reference alone

- Remote URL: `https://example.com/skills/advocate/scripts/check.sh`
- URL query value: `https://example.com/?next=skills/advocate/scripts/check.sh`
- URL fragment: `https://example.com/archive#skills/advocate/scripts/check.sh`
- Punctuation prefix: `archive#skills/advocate/scripts/check.sh`
- Parenthesis inside a URL path: `https://example.com/(skills/advocate/scripts/check.sh)`
- Bracket inside a URL query: `https://example.com/?next=[skills/advocate/scripts/check.sh]`
- Parenthesis inside a filename: `archive(skills/advocate/scripts/check.sh)`
- Brace inside a filename: `archive{skills/advocate/scripts/check.sh}`
- Label close inside a filename: `archive](skills/advocate/scripts/check.sh)`
- Label close inside a URL: `https://example.com/a](skills/advocate/scripts/check.sh)`
- Apostrophe inside a URL: `https://example.com/a'skills/advocate/scripts/check.sh`
- Apostrophe inside a filename: `archive'skills/advocate/scripts/check.sh`
- Label opened inside a word: `note[label](skills/advocate/scripts/check.sh)`
- Non-ASCII prefix: `caféskills/advocate/scripts/check.sh`
- Interior segment: `vendor/skills/advocate/scripts/check.sh`
- Longer directory name: `myskills/advocate/scripts/check.sh`
- Sibling directory name: `skills/advocate-archive/scripts/check.sh`
- Absolute path: `/skills/advocate/scripts/check.sh`
- Explicit relative path: `./skills/advocate/scripts/check.sh`
- Another package's identity, same skill path: `.tessl/plugins/other-workspace/other-plugin/skills/advocate/scripts/check.sh`
- Another package's skill: `.tessl/plugins/other-workspace/other-plugin/skills/unrelated/check.sh`
- Tessl path missing the package segment: `.tessl/plugins/legacy-workspace/skills/advocate/scripts/check.sh`
- Whitespace in the identity: `.tessl/plugins/legacy-workspace bad/advocate-plugin/skills/advocate/scripts/check.sh`
- Skill root without a trailing separator: `skills/advocate`
- Single quotes inside a quoted argument: `"archive 'nested' skills/advocate/scripts/check.sh"`
- Double quotes inside a single-quoted argument: `'archive "nested" skills/advocate/scripts/check.sh'`
- Escaped quote inside a quoted argument: `"archive \" skills/advocate/scripts/check.sh"`
- Quoted assignment value that is not the reference: `OPAQUE_HELPER="archive skills/advocate/scripts/check.sh"`

## Step 5 — Leave a quoted argument's interior alone

    "archive
     skills/advocate/scripts/check.sh"

## Step 6 — Leave adjacent program literals alone

    mount = (".tessl/plugins/legacy-workspace/advocate-plugin"
             "/skills/advocate/scripts/check.sh")

## Step 7 — Leave prose between identity slashes alone

    .tessl/plugins/
    KEEP THIS PROSE
    /advocate-plugin/skills/advocate/scripts/check.sh

## Step 8 — Leave a label separated from a destination by a blank line alone

[label

archive](skills/advocate/scripts/check.sh)

Finish here.
