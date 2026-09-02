package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

const (
	// CodeReleaseExists means an immutable visible version already exists.
	CodeReleaseExists = "release_already_exists"
	// CodeTagCommit means the release tag does not identify the workflow commit.
	CodeTagCommit = "tag_commit_mismatch"
	// CodeTagNotPushed means GitHub has no matching release tag.
	CodeTagNotPushed = "tag_not_pushed"
	// CodeForeignDraft means a same-tag draft contains unrecognized state.
	CodeForeignDraft = "foreign_draft_release"
	// CodeReleaseUpload means draft creation, upload, or publication failed.
	CodeReleaseUpload = "release_upload_failed"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Error is a stable CLI-release refusal with a machine-readable code.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (err *Error) Error() string { return err.Message }

// Unwrap exposes the underlying GitHub or validation failure.
func (err *Error) Unwrap() error { return err.Cause }

// Remote is the GitHub release boundary used by guard and publish operations.
type Remote interface {
	LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error)
	TagCommit(context.Context, dependency.Repository, string) (string, bool, error)
	CreateRelease(context.Context, dependency.Repository, string, string) (dependency.Release, error)
	UploadAsset(context.Context, dependency.Repository, int64, string, string, []byte) (dependency.ReleaseAsset, []byte, error)
	PublishRelease(context.Context, dependency.Repository, int64) (dependency.Release, error)
	DeleteRelease(context.Context, dependency.Repository, int64) error
}

// GuardResult records the immutable identity approved before any build starts.
type GuardResult struct {
	Tag          string `json:"tag"`
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	StaleDraftID int64  `json:"staleDraftId,omitempty"`
}

// PublishResult identifies the newly visible release.
type PublishResult struct {
	Tag        string   `json:"tag"`
	Version    string   `json:"version"`
	Commit     string   `json:"commit"`
	Assets     []string `json:"assets"`
	ReleaseID  int64    `json:"releaseId"`
	ReleaseURL string   `json:"releaseUrl"`
}

// Guard verifies canonical tag identity and refuses any immutable version
// already represented by a visible GitHub Release.
func Guard(ctx context.Context, remote Remote, repository dependency.Repository, tag, commit string) (GuardResult, error) {
	version, err := releaseVersion(tag)
	if err != nil {
		return GuardResult{}, err
	}
	if !fullCommitPattern.MatchString(commit) {
		return GuardResult{}, refusal(CodeTagCommit, "release commit %q is not a full lowercase Git SHA; use github.sha from the tag workflow", commit)
	}
	remoteCommit, exists, err := remote.TagCommit(ctx, repository, tag)
	if err != nil {
		return GuardResult{}, refusalWith(err, CodeReleaseUpload, "probe pushed tag %q: %v", tag, err)
	}
	if !exists {
		return GuardResult{}, refusal(CodeTagNotPushed, "tag %s is not pushed to GitHub; push an immutable tag and retry", tag)
	}
	if remoteCommit != commit {
		return GuardResult{}, refusal(CodeTagCommit, "pushed tag %s resolves to %s instead of workflow commit %s; restore the tag or publish a new version", tag, remoteCommit, commit)
	}
	existing, exists, err := remote.LookupRelease(ctx, repository, tag)
	if err != nil {
		return GuardResult{}, refusalWith(err, CodeReleaseUpload, "probe existing release %q: %v", tag, err)
	}
	result := GuardResult{Tag: tag, Version: version, Commit: commit}
	if !exists {
		return result, nil
	}
	if !existing.Draft {
		return GuardResult{}, refusal(CodeReleaseExists, "release %s already exists and was not overwritten; create and push a new semantic version tag", tag)
	}
	if existing.Target != commit {
		return GuardResult{}, refusal(CodeForeignDraft, "draft release %s targets %q instead of workflow commit %s and was not changed; inspect or remove it manually before retrying", tag, existing.Target, commit)
	}
	if reason := foreignDraftReason(existing.Assets); reason != "" {
		return GuardResult{}, refusal(CodeForeignDraft, "draft release %s %s and was not changed; inspect or remove it manually before retrying", tag, reason)
	}
	result.StaleDraftID = existing.ID
	return result, nil
}

// Publish validates, uploads, re-reads, and exposes exactly one seven-asset
// release. Every failure after creation leaves the release as a draft.
func Publish(ctx context.Context, remote Remote, repository dependency.Repository, tag, commit string, assets []Asset) (PublishResult, error) {
	version, err := releaseVersion(tag)
	if err != nil {
		return PublishResult{}, err
	}
	ordered, err := validateCompleteAssets(version, assets)
	if err != nil {
		return PublishResult{}, err
	}
	guarded, err := Guard(ctx, remote, repository, tag, commit)
	if err != nil {
		return PublishResult{}, err
	}
	if guarded.StaleDraftID != 0 {
		if err := remote.DeleteRelease(ctx, repository, guarded.StaleDraftID); err != nil {
			return PublishResult{}, refusalWith(err, CodeReleaseUpload, "delete stale acr draft release %s: %v", tag, err)
		}
	}
	draft, err := remote.CreateRelease(ctx, repository, tag, commit)
	if err != nil {
		if dependency.IsGitHubStatus(err, http.StatusUnprocessableEntity) {
			return PublishResult{}, refusalWith(err, CodeReleaseExists, "release %s already exists and was not overwritten; create and push a new semantic version tag", tag)
		}
		return PublishResult{}, refusalWith(err, CodeReleaseUpload, "create draft release %s: %v", tag, err)
	}
	for _, asset := range ordered {
		_, remoteBytes, err := remote.UploadAsset(ctx, repository, draft.ID, asset.Name, asset.ContentType, asset.Bytes)
		if err != nil {
			return PublishResult{}, refusalWith(err, CodeReleaseUpload, "upload release asset %q: %v; release %s remains a draft", asset.Name, err, tag)
		}
		if !bytes.Equal(remoteBytes, asset.Bytes) {
			return PublishResult{}, refusal(CodeReleaseUpload, "uploaded release asset %q differs from the prepared bytes; release %s remains a draft", asset.Name, tag)
		}
	}
	remoteCommit, exists, err := remote.TagCommit(ctx, repository, tag)
	if err != nil {
		return PublishResult{}, refusalWith(err, CodeReleaseUpload, "recheck pushed tag %q before publication: %v; release remains a draft", tag, err)
	}
	if !exists {
		return PublishResult{}, refusal(CodeTagNotPushed, "tag %s disappeared before publication; release remains a draft", tag)
	}
	if remoteCommit != commit {
		return PublishResult{}, refusal(CodeTagCommit, "pushed tag %s moved to %s before publication instead of %s; release remains a draft", tag, remoteCommit, commit)
	}
	published, err := remote.PublishRelease(ctx, repository, draft.ID)
	if err != nil {
		return PublishResult{}, refusalWith(err, CodeReleaseUpload, "publish verified draft release %s: %v; retry while it remains a draft", tag, err)
	}
	if published.ID != draft.ID || published.Draft || published.Prerelease || published.Tag != tag {
		return PublishResult{}, refusal(CodeReleaseUpload, "GitHub returned non-final metadata for release %s; inspect the release before retrying", tag)
	}
	names := make([]string, len(ordered))
	for index, asset := range ordered {
		names[index] = asset.Name
	}
	return PublishResult{
		Tag: tag, Version: version, Commit: commit, Assets: names,
		ReleaseID: published.ID, ReleaseURL: published.HTMLURL,
	}, nil
}

// ExpectedAssetNames returns the complete visible release set in upload order.
func ExpectedAssetNames() []string {
	names := []string{ChecksumsAssetName, SignatureAssetName, SBOMAssetName}
	for _, target := range Targets() {
		names = append(names, target.Name())
	}
	sort.Strings(names)
	return names
}

func validateCompleteAssets(version string, assets []Asset) ([]Asset, error) {
	expectedNames := ExpectedAssetNames()
	expected := make(map[string]struct{}, len(expectedNames))
	for _, name := range expectedNames {
		expected[name] = struct{}{}
	}
	byName := make(map[string]Asset, len(assets))
	for _, asset := range assets {
		if _, ok := expected[asset.Name]; !ok {
			return nil, refusal(CodeReleaseUpload, "release asset %q is outside the seven-asset acr contract; remove it and retry", asset.Name)
		}
		if _, duplicate := byName[asset.Name]; duplicate {
			return nil, refusal(CodeReleaseUpload, "release asset %q appears more than once; provide each asset exactly once", asset.Name)
		}
		if len(asset.Bytes) == 0 {
			return nil, refusal(CodeReleaseUpload, "release asset %q is empty; regenerate it and retry", asset.Name)
		}
		byName[asset.Name] = asset
	}
	for _, name := range expectedNames {
		if _, exists := byName[name]; !exists {
			return nil, refusal(CodeReleaseUpload, "release asset %q is missing; produce all seven assets before publishing", name)
		}
	}
	archives := make([]Asset, 0, len(Targets()))
	for _, target := range Targets() {
		archives = append(archives, byName[target.Name()])
		if _, err := VerifyArchiveChecksum(target.Name(), byName[target.Name()].Bytes, byName[ChecksumsAssetName].Bytes); err != nil {
			return nil, refusalWith(err, CodeReleaseUpload, "%v", err)
		}
	}
	if !bytes.Equal(Checksums(archives), byName[ChecksumsAssetName].Bytes) {
		return nil, refusal(CodeReleaseUpload, "checksums.txt is not the canonical sorted digest manifest for the four archives; regenerate it and retry")
	}
	if !json.Valid(byName[SignatureAssetName].Bytes) {
		return nil, refusal(CodeReleaseUpload, "%s is not JSON; regenerate the keyless signature bundle and retry", SignatureAssetName)
	}
	if err := ValidateSBOM(byName[SBOMAssetName].Bytes, version); err != nil {
		return nil, refusalWith(err, CodeReleaseUpload, "%v", err)
	}
	ordered := make([]Asset, len(expectedNames))
	for index, name := range expectedNames {
		asset := byName[name]
		asset.ContentType = contentType(name)
		ordered[index] = asset
	}
	return ordered, nil
}

func contentType(name string) string {
	switch name {
	case ChecksumsAssetName:
		return "text/plain; charset=utf-8"
	case SignatureAssetName, SBOMAssetName:
		return "application/json"
	default:
		return "application/gzip"
	}
}

func foreignDraftReason(assets []dependency.ReleaseAsset) string {
	allowed := make(map[string]struct{}, len(ExpectedAssetNames()))
	for _, name := range ExpectedAssetNames() {
		allowed[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if _, ok := allowed[asset.Name]; !ok {
			return fmt.Sprintf("contains foreign asset %q", asset.Name)
		}
		if _, duplicate := seen[asset.Name]; duplicate {
			return fmt.Sprintf("contains duplicate asset %q", asset.Name)
		}
		seen[asset.Name] = struct{}{}
	}
	return ""
}

func releaseVersion(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		return "", refusal(CodeTagCommit, "release tag %q is not canonical vMAJOR.MINOR.PATCH; push a stable v-prefixed semantic version tag", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	if !releaseVersionPattern.MatchString(version) || !dependency.TagMatchesVersion(tag, version) {
		return "", refusal(CodeTagCommit, "release tag %q is not canonical vMAJOR.MINOR.PATCH; push a stable semantic version tag", tag)
	}
	return version, nil
}

func refusal(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func refusalWith(cause error, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// IsCode reports whether err carries one stable release refusal code.
func IsCode(err error, code string) bool {
	var releaseErr *Error
	return errors.As(err, &releaseErr) && releaseErr.Code == code
}
