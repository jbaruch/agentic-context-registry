# Journey fixture provenance

The fixtures in `journey_package_test.go` and `journey_github_test.go` are
generated from text at run time; no archive or response body is committed. This
file records where their shape came from, so a reader can tell what was observed
from GitHub and what was written from the API contract, and so a maintainer can
refresh either kind on purpose rather than by editing until the suite goes green.

## Two kinds of fixture, and they are not equally trustworthy

**Observed — the source tarball shape.** Every structural property the source
archive builder reproduces was read from a real GitHub source tarball, recorded
below. If GitHub changes one of them the builder is wrong and the suite will
still pass, which is what the refresh procedure exists to catch.

**Synthetic — every JSON response body.** The release, tag, commit and asset
responses the fixture server returns were written from GitHub's REST
documentation and from the fields `internal/dependency` decodes. They were not
captured from a recorded exchange. A fixture can therefore agree with a mistaken
model of the API, and no deterministic test can notice. Only the live checks in
[`docs/manual-conformance.md`](../../docs/manual-conformance.md) can.

## Observed source archive

One capture, read-only, from a retained archive on 2026-09-04 UTC. It was
downloaded during the FFA dogfood on the same date.

| Field | Value |
| --- | --- |
| Repository | `jbaruch/ffa-acr-dogfood` |
| Tag | `v0.9.38` |
| Commit | `769950e1ab14ad5df4ac2bed45efa6f353a97674` |
| Endpoint | `https://api.github.com/repos/jbaruch/ffa-acr-dogfood/tarball/769950e1ab14ad5df4ac2bed45efa6f353a97674` |
| Observed (UTC) | 2026-09-04 |
| Capture digest | `sha256:efec769d57a3ec592ba311b3f7c370b41390327b87aeed8faafb38e213cb8691` |

Structural properties, each reproduced by `journeyGitHubArchive`:

- A leading `pax_global_header` entry, type `g`, whose `comment` record is the
  commit `769950e1ab14ad5df4ac2bed45efa6f353a97674`.
- Exactly one logical root directory, `jbaruch-ffa-acr-dogfood-769950e`.
- Regular files at `0664` and `0775`; directories at `0775`. No publisher
  records those bits; GitHub applies them.
- First ordered entries, root first, directories before the files they contain:
  `jbaruch-ffa-acr-dogfood-769950e`, `.../.claude`, `.../.claude/skills`,
  `.../.claude/skills/.gitignore` (`0664`), `.../.codex`, `.../.codex/skills`,
  `.../.codex/skills/.gitignore` (`0664`), `.../.env.example` (`0664`).

### Three different hashes, and they answer different questions

Conflating these is the easiest way to write a fixture that proves nothing.

| Hash | What it identifies | Where it appears |
| --- | --- | --- |
| Capture digest | The compressed bytes of one download | This file only. GitHub promises nothing about tarball compression being reproducible, so this identifies a capture and is never an expected value in a test |
| Published asset digest | The bytes of one uploaded release asset | Read back from the upload response by the publisher |
| Canonical content hash | The package's normalized contents, modes included | `agent-plugin.yaml` identity, the release metadata asset, and the consumer's lock. This is the only one two parties are expected to agree on |

The dogfood release also recorded release ID `383002658` and canonical content
hash `sha256:91328738a779e5fe4c330b1f3dac8ef6a3af7091642caa8f981b8c1e6d7f1208`.

### Not captured

Recorded as unknown rather than guessed:

- HTTP response headers of the capture — status line, `content-type`,
  `content-disposition`, `etag`, rate-limit headers. Not retained.
- The redirect chain `api.github.com` served before `codeload.github.com`.
- The full ordered entry list beyond the eight entries above.
- Any release, tag, commit or asset JSON body. None was retained, which is why
  every response the fixture server returns is synthetic.

## Refresh

Refresh on the earlier of: **monthly**, or **any change to a GitHub contract the
fixtures model** — an archive layout or mode change, a new or renamed field in a
response `internal/dependency` decodes, a redirect origin change, or an upload
protocol change.

1. Download the same immutable endpoint above, read-only, into a scratch
   directory. Never commit the artifact; it stays outside version control.
2. Record what this file records: endpoint, observation date, capture digest,
   the global PAX header, the root, the mode sets, and the first ordered
   entries. Record response headers this time; they are listed as unknown above
   because they were not kept.
3. Compare the structural records — entry order, types, modes, PAX header — with
   what `journeyGitHubArchive` builds. Compare structure, never compressed bytes.
4. A difference is a finding about ACR, not about the fixture. Fix the builder
   or the production code, then update this file in the same change.
5. Run `go test -race ./...` before proposing the change, and land the fixture
   edit and the provenance edit together through review.

Never regenerate an expected value from the code under test, and never make a
red test green by refreshing a fixture. A refresh changes recorded observations;
it never changes an assertion to match a new output.
