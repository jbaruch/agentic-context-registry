package release

import (
	"context"
	"os/exec"
	"strings"
)

// CodeReleaseNotes means the tag has no release notes in its annotation body.
const CodeReleaseNotes = "release_notes_missing"

// RequireTagReleaseNotes returns the fetched tag object's SHA after checking
// its target commit and annotation body. Git excludes the subject and any
// signature from the body used for the notes check.
func RequireTagReleaseNotes(ctx context.Context, tag, commit string) (string, error) {
	if _, err := releaseVersion(tag); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "git", "--no-replace-objects", "tag", "--list",
		"--format=%(objectname)%00%(*objecttype)%00%(*objectname)%00%(contents:body)", "--", tag)
	output, err := command.Output()
	if err != nil {
		return "", refusalWith(err, CodeTagCommit, "read local tag %s: %v; check the tagged checkout and retry", tag, err)
	}
	if len(output) == 0 {
		return "", refusal(CodeTagCommit, "tag %s is missing from the local checkout; fetch the release tag and retry", tag)
	}
	fields := strings.SplitN(string(output), "\x00", 4)
	if len(fields) != 4 || !fullSHAPattern.MatchString(fields[0]) {
		return "", refusal(CodeTagCommit, "Git returned unexpected annotation data for tag %s; check the tagged checkout and retry", tag)
	}
	objectSHA, targetType, targetSHA, body := fields[0], fields[1], fields[2], fields[3]
	if targetType == "" || strings.TrimSpace(body) == "" {
		return "", refusal(CodeReleaseNotes, "tag %s has no release notes; create an annotated release tag with notes after a blank line below its subject", tag)
	}
	if targetType != "commit" || targetSHA != commit {
		return "", refusal(CodeTagCommit, "local tag %s resolves to %s %q. Expected workflow commit %q. Check the tagged checkout and retry", tag, targetType, targetSHA, commit)
	}
	return objectSHA, nil
}
