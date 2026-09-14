package release

import (
	"bufio"
	"bytes"
	"strings"
)

// CodeChangelogHeading means CHANGELOG.md carries no heading for the version
// the pushed tag names.
const CodeChangelogHeading = "changelog_heading_missing"

// RequireChangelogVersion refuses a release whose version has no `## <version>`
// heading in the changelog, so a tag cannot ship with its entries still sitting
// under a placeholder heading. The tag supplies the version through the same
// canonical derivation the rest of the release path uses, and a heading matches
// when its first token after `## ` is exactly that version: both
// `## 0.2.0 — 2026-09-14` and a bare `## 0.2.0` satisfy it.
func RequireChangelogVersion(changelog []byte, tag string) error {
	version, err := releaseVersion(tag)
	if err != nil {
		return err
	}
	if changelogHasVersion(changelog, version) {
		return nil
	}
	return refusal(CodeChangelogHeading,
		"CHANGELOG.md has no `## %s` heading for tag %s; add the version heading above that release's entries and push the tag again",
		version, tag)
}

// changelogHasVersion reports whether any `## ` heading names exactly version.
// Fenced regions are skipped so an entry quoting a heading cannot satisfy the
// check for a version the changelog never actually documents.
func changelogHasVersion(changelog []byte, version string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(changelog))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	fenced := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		heading, ok := strings.CutPrefix(line, "## ")
		if !ok {
			continue
		}
		if name, _, _ := strings.Cut(strings.TrimSpace(heading), " "); name == version {
			return true
		}
	}
	return false
}
