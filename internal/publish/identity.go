package publish

import (
	"context"
	"fmt"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

const (
	CodeNoPublishableTag = "no_publishable_tag"
	CodeDirtyWorktree    = "dirty_worktree"
	CodeAmbiguousTag     = "ambiguous_tag"
	CodeTagVersion       = "tag_version_mismatch"
	CodeUnpublishable    = "unpublishable_path"
	CodeReleaseExists    = "release_already_exists"
	CodeTagCommit        = "tag_commit_mismatch"
	CodeTagNotPushed     = "tag_not_pushed"
	CodeForeignDraft     = "foreign_draft_release"
	CodeReleaseUpload    = "release_upload_failed"
)

// Error is a stable publisher refusal with a machine-readable code.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (err *Error) Error() string { return err.Message }

// Unwrap exposes the underlying Git or filesystem failure.
func (err *Error) Unwrap() error { return err.Cause }

// Identity binds one publication to the tagged commit at HEAD.
type Identity struct {
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
}

func resolveIdentity(ctx context.Context, root, version string, source gitSource) (Identity, error) {
	clean, err := source.Clean(ctx, root)
	if err != nil {
		return Identity{}, publishError(CodeDirtyWorktree, "inspect Git worktree: %v; verify Git can read the package repository and retry", err)
	}
	if !clean {
		return Identity{}, publishError(CodeDirtyWorktree, "Git worktree has uncommitted changes; commit or remove every change before publishing")
	}
	commit, err := source.Head(ctx, root)
	if err != nil {
		return Identity{}, publishError(CodeNoPublishableTag, "resolve Git HEAD: %v; publish from a committed package repository", err)
	}
	tags, err := source.TagsAtHead(ctx, root)
	if err != nil {
		return Identity{}, publishError(CodeNoPublishableTag, "inspect tags at HEAD: %v; fetch tags and retry", err)
	}
	switch len(tags) {
	case 0:
		return Identity{}, publishError(CodeNoPublishableTag, "HEAD %s has no publishable tag; create tag v%s at HEAD and retry", commit, version)
	case 1:
	default:
		return Identity{}, publishError(CodeAmbiguousTag, "HEAD %s has multiple tags (%s); leave exactly one package version tag at HEAD", commit, strings.Join(tags, ", "))
	}
	if !dependency.TagMatchesVersion(tags[0], version) {
		return Identity{}, publishError(CodeTagVersion, "Git tag %q does not match manifest version %q; use %s or v%s", tags[0], version, version, version)
	}
	return Identity{Tag: tags[0], Commit: commit}, nil
}

func publishError(code, format string, args ...any) *Error {
	message := fmt.Sprintf(format, args...)
	var cause error
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			cause = err
			break
		}
	}
	return &Error{Code: code, Message: message, Cause: cause}
}
