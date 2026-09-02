package publishapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/publish"
)

type preparer interface {
	Prepare(context.Context, string) (publish.Prepared, error)
}

// Service runs publication stages P1-P7 while keeping all GitHub writes after
// validation, realization, and immutability probes.
type Service struct {
	preparer preparer
	releases releases
}

// NewService constructs publication operations.
func NewService(preparer preparer, releases releases) *Service {
	return &Service{preparer: preparer, releases: releases}
}

// Result describes a dry-run plan or completed immutable release.
type Result struct {
	DryRun      bool     `json:"dryRun"`
	Tag         string   `json:"tag"`
	Commit      string   `json:"commit"`
	ContentHash string   `json:"contentHash"`
	Assets      []string `json:"assets"`
	ReleaseID   int64    `json:"releaseId,omitempty"`
	ReleaseURL  string   `json:"releaseUrl,omitempty"`
}

// Publish validates and gates a package, probes immutable remote state, and
// uploads through a draft that becomes visible only after byte verification.
func (service *Service) Publish(ctx context.Context, root string, dryRun bool) (Result, error) {
	prepared, err := service.preparer.Prepare(ctx, root)
	if err != nil {
		return Result{}, err
	}
	repository, err := dependency.ParseSource("github:" + prepared.Manifest.Name)
	if err != nil {
		return Result{}, err
	}
	result := publicationResult(prepared, dryRun)
	existing, exists, err := service.releases.LookupRelease(ctx, repository, prepared.Identity.Tag)
	if err != nil {
		return Result{}, refusal(publish.CodeReleaseUpload, "probe existing release %q: %v", prepared.Identity.Tag, err)
	}
	staleDraft := false
	if exists {
		if !existing.Draft {
			return Result{}, refusal(publish.CodeReleaseExists, "release %s already exists and was not overwritten; bump version in agent-plugin.yaml and tag the new commit", prepared.Identity.Tag)
		}
		if foreign := foreignAsset(existing.Assets, result.Assets); foreign != "" {
			return Result{}, refusal(publish.CodeForeignDraft, "draft release %s contains foreign asset %q and was not changed; remove or rename the draft manually before retrying", prepared.Identity.Tag, foreign)
		}
		staleDraft = true
	}
	remoteCommit, pushed, err := service.releases.TagCommit(ctx, repository, prepared.Identity.Tag)
	if err != nil {
		return Result{}, refusal(publish.CodeReleaseUpload, "probe pushed tag %q: %v", prepared.Identity.Tag, err)
	}
	if !pushed {
		return Result{}, refusal(publish.CodeTagNotPushed, "tag %s is not pushed to GitHub; push the tag and retry", prepared.Identity.Tag)
	}
	if remoteCommit != prepared.Identity.Commit {
		return Result{}, refusal(publish.CodeTagCommit, "pushed tag %s resolves to %s instead of local HEAD %s; restore the tag or publish a new version", prepared.Identity.Tag, remoteCommit, prepared.Identity.Commit)
	}
	if dryRun {
		return result, nil
	}
	if staleDraft {
		if err := service.releases.DeleteRelease(ctx, repository, existing.ID); err != nil {
			return Result{}, refusal(publish.CodeReleaseUpload, "delete stale ACR draft release %s: %v", prepared.Identity.Tag, err)
		}
	}
	draft, err := service.releases.CreateRelease(ctx, repository, prepared.Identity.Tag, prepared.Identity.Commit)
	if err != nil {
		if dependency.IsGitHubStatus(err, http.StatusUnprocessableEntity) {
			return Result{}, refusal(publish.CodeReleaseExists, "release %s already exists and was not overwritten; bump version in agent-plugin.yaml and tag the new commit", prepared.Identity.Tag)
		}
		return Result{}, refusal(publish.CodeReleaseUpload, "create draft release %s: %v", prepared.Identity.Tag, err)
	}
	for _, asset := range []publish.Asset{prepared.Assets.Archive, prepared.Assets.Metadata, prepared.Assets.Checksums} {
		_, remoteBytes, err := service.releases.UploadAsset(ctx, repository, draft.ID, asset.Name, asset.ContentType, asset.Bytes)
		if err != nil {
			return Result{}, refusal(publish.CodeReleaseUpload, "upload release asset %q: %v; release %s remains a draft", asset.Name, err, prepared.Identity.Tag)
		}
		localDigest := sha256.Sum256(asset.Bytes)
		remoteDigest := sha256.Sum256(remoteBytes)
		if !bytes.Equal(localDigest[:], remoteDigest[:]) {
			return Result{}, refusal(publish.CodeReleaseUpload, "uploaded release asset %q has SHA-256 %s, expected %s; release %s remains a draft", asset.Name, hex.EncodeToString(remoteDigest[:]), hex.EncodeToString(localDigest[:]), prepared.Identity.Tag)
		}
	}
	published, err := service.releases.PublishRelease(ctx, repository, draft.ID)
	if err != nil {
		return Result{}, refusal(publish.CodeReleaseUpload, "publish verified draft release %s: %v; retry while it remains a draft", prepared.Identity.Tag, err)
	}
	if published.ID != draft.ID || published.Draft || published.Prerelease {
		return Result{}, refusal(publish.CodeReleaseUpload, "GitHub returned non-final metadata for release %s; inspect the release before retrying", prepared.Identity.Tag)
	}
	result.ReleaseID = published.ID
	result.ReleaseURL = published.HTMLURL
	return result, nil
}

func publicationResult(prepared publish.Prepared, dryRun bool) Result {
	assets := []string{prepared.Assets.Archive.Name, prepared.Assets.Metadata.Name, prepared.Assets.Checksums.Name}
	sort.Strings(assets)
	return Result{
		DryRun: dryRun, Tag: prepared.Identity.Tag, Commit: prepared.Identity.Commit,
		ContentHash: prepared.Assets.Evidence.ContentHash, Assets: assets,
	}
}

func foreignAsset(assets []dependency.ReleaseAsset, allowed []string) string {
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}
	for _, asset := range assets {
		if _, ok := set[asset.Name]; !ok {
			return asset.Name
		}
	}
	return ""
}

func refusal(code, format string, args ...any) *publish.Error {
	return &publish.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
