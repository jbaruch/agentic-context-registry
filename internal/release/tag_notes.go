package release

import (
	"context"
	"os/exec"
	"strings"
)

// CodeReleaseNotes means the tag has no release notes in its annotation body.
const CodeReleaseNotes = "release_notes_missing"

// RequireTagReleaseNotes checks that the fetched tag resolves to the workflow
// commit and has release notes in its annotation before the release guard probes
// GitHub. The subject can name the release. The body after the subject records
// the notes, and Git excludes any signature from that body.
func RequireTagReleaseNotes(ctx context.Context, tag, commit string) error {
	if _, err := releaseVersion(tag); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", "--no-replace-objects", "tag", "--list",
		"--format=%(*objecttype)%00%(*objectname)%00%(contents:body)", "--", tag)
	output, err := command.Output()
	if err != nil {
		return refusalWith(err, CodeTagCommit, "read local tag %s: %v; check the tagged checkout and retry", tag, err)
	}
	if len(output) == 0 {
		return refusal(CodeTagCommit, "tag %s is missing from the local checkout; fetch the release tag and retry", tag)
	}
	fields := strings.SplitN(string(output), "\x00", 3)
	if len(fields) != 3 {
		return refusal(CodeTagCommit, "Git returned unexpected annotation data for tag %s; check the tagged checkout and retry", tag)
	}
	if fields[0] == "" || strings.TrimSpace(fields[2]) == "" {
		return refusal(CodeReleaseNotes, "tag %s has no release notes; create an annotated release tag with notes after a blank line below its subject", tag)
	}
	if fields[0] != "commit" || fields[1] != commit {
		return refusal(CodeTagCommit, "local tag %s resolves to %s %q. Expected workflow commit %q. Check the tagged checkout and retry", tag, fields[0], fields[1], commit)
	}
	return nil
}
