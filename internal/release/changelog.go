package release

import (
	"bufio"
	"bytes"
	"strings"
)

// CodeChangelogHeading means CHANGELOG.md carries no heading for the version
// the pushed tag names.
const CodeChangelogHeading = "changelog_heading_missing"

// maxLeadingSpaces is the CommonMark limit on indentation before a heading or a
// code fence. A fourth space starts an indented code block instead.
const maxLeadingSpaces = 3

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
	var fenceChar byte
	fenceLen := 0
	for scanner.Scan() {
		line, indented := stripLeadingSpaces(strings.TrimRight(scanner.Text(), " \t"))
		if indented {
			continue
		}
		char, run, trailing := fenceMarker(line)
		if fenceLen == 0 {
			// A fence opens on any run of three or more of either marker.
			if run >= 3 {
				fenceChar, fenceLen = char, run
			}
			if run == 0 {
				if name, ok := headingVersion(line); ok && name == version {
					return true
				}
			}
			continue
		}
		// Inside a fence only a run of the same marker, at least as long as the
		// opener and carrying no trailing content, closes it.
		if char == fenceChar && run >= fenceLen && trailing == "" {
			fenceChar, fenceLen = 0, 0
		}
	}
	return false
}

// stripLeadingSpaces removes up to three leading spaces and reports whether the
// line carried more, which makes it indented content rather than markup.
func stripLeadingSpaces(line string) (string, bool) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces > maxLeadingSpaces {
		return line, true
	}
	return line[spaces:], false
}

// fenceMarker returns the leading code-fence marker, the length of its run, and
// whatever follows it. A line that opens with neither marker has a zero run.
func fenceMarker(line string) (char byte, run int, trailing string) {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0, ""
	}
	char = line[0]
	for run < len(line) && line[run] == char {
		run++
	}
	return char, run, strings.TrimSpace(line[run:])
}

// headingVersion returns the first token of a level-two heading.
func headingVersion(line string) (string, bool) {
	heading, ok := strings.CutPrefix(line, "## ")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimSpace(heading), " ")
	return name, true
}
