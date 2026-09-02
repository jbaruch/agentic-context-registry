package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Resolver turns requested policies into immutable verified locks.
type Resolver struct {
	github        GitHub
	warningWriter io.Writer
	previewMu     sync.RWMutex
	vendorPreview map[string]MaterializedPackage
}

// NewResolver constructs a dependency resolver using the supplied GitHub API.
func NewResolver(github GitHub) *Resolver {
	return newResolver(github, os.Stderr)
}

func newResolver(github GitHub, warningWriter io.Writer) *Resolver {
	return &Resolver{github: github, warningWriter: warningWriter, vendorPreview: make(map[string]MaterializedPackage)}
}

// Resolve resolves and verifies one declaration. It is the composition of
// Candidate and ResolveAt for callers that do not already hold a candidate.
func (resolver *Resolver) Resolve(ctx context.Context, declaration Declaration) (LockedDependency, error) {
	release, err := resolver.Candidate(ctx, declaration)
	if err != nil {
		return LockedDependency{}, err
	}
	return resolver.ResolveAt(ctx, declaration, release)
}

// Candidate resolves only the release metadata a declaration selects, without
// downloading or verifying package content. A commit request selects no
// release and returns the zero Release, which ResolveAt ignores.
func (resolver *Resolver) Candidate(ctx context.Context, declaration Declaration) (Release, error) {
	repository, err := ParseSource(declaration.Source)
	if err != nil {
		return Release{}, err
	}
	if err := validateRequested(declaration.Requested); err != nil {
		return Release{}, fmt.Errorf("invalid requested policy %q for %s: %w", declaration.Requested, declaration.Source, err)
	}
	switch {
	case declaration.Requested == "latest":
		return resolver.github.LatestRelease(ctx, repository)
	case isCommitRequest(declaration.Requested):
		return Release{}, nil
	default:
		return resolver.github.ReleaseByTag(ctx, repository, declaration.Requested)
	}
}

// ResolveAt resolves and verifies one declaration against an already-fetched
// candidate, so a caller that needed the candidate for its own decision pays
// for it once.
func (resolver *Resolver) ResolveAt(ctx context.Context, declaration Declaration, release Release) (LockedDependency, error) {
	repository, err := ParseSource(declaration.Source)
	if err != nil {
		return LockedDependency{}, err
	}
	if err := validateRequested(declaration.Requested); err != nil {
		return LockedDependency{}, fmt.Errorf("invalid requested policy %q for %s: %w", declaration.Requested, declaration.Source, err)
	}
	if isCommitRequest(declaration.Requested) {
		commit, err := resolver.github.ResolveCommit(ctx, repository, declaration.Requested)
		if err != nil {
			return LockedDependency{}, err
		}
		locked := LockedDependency{Source: declaration.Source, Requested: declaration.Requested, Kind: ResolutionCommit, Commit: commit}
		return resolver.finishResolution(ctx, repository, locked)
	}
	if release.ID <= 0 || release.Tag == "" || release.Draft || release.Prerelease {
		return LockedDependency{}, fmt.Errorf("release candidate for %s is not stable; publish a stable GitHub Release and retry", declaration.Source)
	}
	commit, err := resolver.github.ResolveCommit(ctx, repository, release.Tag)
	if err != nil {
		return LockedDependency{}, err
	}
	locked := LockedDependency{
		Source: declaration.Source, Requested: declaration.Requested, Kind: ResolutionRelease,
		ReleaseID: release.ID, Tag: release.Tag, Commit: commit,
	}
	return resolver.finishReleaseResolution(ctx, repository, release, locked)
}

// LatestRelease resolves only the mutable stable release candidate.
func (resolver *Resolver) LatestRelease(ctx context.Context, source string) (Release, error) {
	repository, err := ParseSource(source)
	if err != nil {
		return Release{}, err
	}
	return resolver.github.LatestRelease(ctx, repository)
}

func (resolver *Resolver) finishResolution(ctx context.Context, repository Repository, locked LockedDependency) (LockedDependency, error) {
	archive, err := resolver.github.DownloadArchive(ctx, repository, locked.Commit)
	if err != nil {
		return LockedDependency{}, err
	}
	verified, err := verifyPackageArchive(archive, repository)
	if err != nil {
		return LockedDependency{}, err
	}
	if locked.Kind == ResolutionRelease && !TagMatchesVersion(locked.Tag, verified.Version) {
		return LockedDependency{}, fmt.Errorf("release tag %q does not match package version %q; publish matching agent-plugin.yaml metadata and retry", locked.Tag, verified.Version)
	}
	locked.PackageVersion = verified.Version
	locked.ContentHash = verified.ContentHash
	return locked, nil
}

func (resolver *Resolver) finishReleaseResolution(ctx context.Context, repository Repository, release Release, locked LockedDependency) (LockedDependency, error) {
	locked, err := resolver.finishResolution(ctx, repository, locked)
	if err != nil {
		return LockedDependency{}, err
	}
	if err := resolver.verifyReleaseMetadata(ctx, repository, release, locked.Commit, locked.ContentHash); err != nil {
		return LockedDependency{}, err
	}
	return locked, nil
}

func (resolver *Resolver) verifyReleaseMetadata(ctx context.Context, repository Repository, release Release, commit, contentHash string) error {
	var metadataAsset *ReleaseAsset
	for index := range release.Assets {
		if release.Assets[index].Name == ReleaseMetadataAssetName {
			asset := release.Assets[index]
			metadataAsset = &asset
			break
		}
	}
	if metadataAsset == nil {
		return nil
	}
	contents, err := resolver.github.DownloadReleaseAsset(ctx, repository, *metadataAsset)
	if err != nil {
		// Release metadata is additive evidence. Repositories published before
		// this contract, and temporarily unavailable assets, retain the existing
		// source-tree installation path.
		return resolver.warnIgnoredReleaseMetadata(release.Tag, fmt.Sprintf("download failed: %v", err))
	}
	var metadata struct {
		MetadataVersion int    `json:"metadataVersion"`
		Commit          string `json:"commit"`
		ContentHash     string `json:"contentHash"`
	}
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return resolver.warnIgnoredReleaseMetadata(release.Tag, fmt.Sprintf("malformed JSON: %v", err))
	}
	if metadata.MetadataVersion != 1 {
		return resolver.warnIgnoredReleaseMetadata(release.Tag, fmt.Sprintf("unsupported metadataVersion %d", metadata.MetadataVersion))
	}
	if metadata.Commit != commit {
		return fmt.Errorf("release metadata commit mismatch for %s tag %s: metadata records %s, resolved tag points to %s; do not use the package and restore the immutable tag", repository.String(), release.Tag, metadata.Commit, commit)
	}
	if metadata.ContentHash != contentHash {
		return fmt.Errorf("release metadata content hash mismatch for %s tag %s: metadata records %s, source tree verifies as %s; do not use the package and inspect the release", repository.String(), release.Tag, metadata.ContentHash, contentHash)
	}
	return nil
}

func (resolver *Resolver) warnIgnoredReleaseMetadata(tag, reason string) error {
	if _, err := fmt.Fprintf(resolver.warningWriter, "warning: ignored release metadata for tag %q: %s\n", tag, reason); err != nil {
		return fmt.Errorf("write release metadata warning for tag %q: %w", tag, err)
	}
	return nil
}

// VerifyLocked downloads an immutable locked commit without resolving a tag or
// release and verifies both its package version and canonical content hash.
func (resolver *Resolver) VerifyLocked(ctx context.Context, locked LockedDependency) error {
	_, cleanup, err := resolver.MaterializeLocked(ctx, locked)
	if err != nil {
		return err
	}
	return cleanup()
}

// MaterializedPackage is a verified immutable package retained in temporary
// storage for the duration of native rendering.
type MaterializedPackage struct {
	Root     string
	Manifest manifest.Manifest
}

// MaterializeLocked downloads and verifies one lock without consulting
// mutable release metadata. The caller must invoke cleanup after rendering.
func (resolver *Resolver) MaterializeLocked(ctx context.Context, locked LockedDependency) (result MaterializedPackage, cleanup func() error, err error) {
	if locked.Kind == ResolutionVendor {
		return MaterializedPackage{}, nil, fmt.Errorf("vendored dependency %s requires a project root; realize it from the owning project", locked.Source)
	}
	repository, err := ParseSource(locked.Source)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	if len(locked.Commit) != 40 || !isCommitRequest(locked.Commit) {
		return MaterializedPackage{}, nil, fmt.Errorf("locked commit %q for %s is invalid; run 'acr install' to regenerate %s", locked.Commit, locked.Source, LockFilename)
	}
	archive, err := resolver.github.DownloadArchive(ctx, repository, locked.Commit)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	root, err := os.MkdirTemp("", "acr-package-*")
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("create package materialization directory: %w; verify temporary storage is writable and retry", err)
	}
	remove := func() error { return os.RemoveAll(root) }
	defer func() {
		if err != nil {
			err = errors.Join(err, remove())
		}
	}()
	if err = ExtractPackageArchive(archive, root); err != nil {
		return MaterializedPackage{}, nil, err
	}
	value, err := manifest.Load(root)
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("validate downloaded %s package: %w; fix the package manifest and publish a new release", repository.String(), err)
	}
	if value.Name != repository.FullName() || value.Source.Repository != "https://github.com/"+repository.FullName() {
		return MaterializedPackage{}, nil, fmt.Errorf("downloaded package identity %q does not match %s; fix agent-plugin.yaml and publish a new release", value.Name, repository.String())
	}
	contentHash, err := HashPackageFiles(root, value)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	if value.Version != locked.PackageVersion {
		return MaterializedPackage{}, nil, fmt.Errorf("locked package version mismatch for %s: expected %s, downloaded %s; remove %s and run 'acr install'", locked.Source, locked.PackageVersion, value.Version, LockFilename)
	}
	if contentHash != locked.ContentHash {
		return MaterializedPackage{}, nil, fmt.Errorf("content hash mismatch for %s at %s: expected %s, downloaded %s; do not use the package and verify the repository contents", locked.Source, locked.Commit, locked.ContentHash, contentHash)
	}
	return MaterializedPackage{Root: root, Manifest: value}, remove, nil
}

// MaterializeLockedAt materializes a lock in the context of its owning
// project. GitHub packages retain temporary cleanup; vendor packages return
// their persistent tree and a no-op cleanup.
func (resolver *Resolver) MaterializeLockedAt(ctx context.Context, projectDirectory string, locked LockedDependency) (MaterializedPackage, func() error, error) {
	if locked.Kind == ResolutionVendor {
		resolver.previewMu.RLock()
		preview, ok := resolver.vendorPreview[locked.Source]
		resolver.previewMu.RUnlock()
		if ok {
			return preview, func() error { return nil }, nil
		}
		return materializeVendor(projectDirectory, locked)
	}
	return resolver.MaterializeLocked(ctx, locked)
}

// RegisterVendorPreview supplies a verified source tree for one migration
// preview. ClearVendorPreviews must be called after the operation.
func (resolver *Resolver) RegisterVendorPreview(source, root string, value manifest.Manifest) {
	resolver.previewMu.Lock()
	defer resolver.previewMu.Unlock()
	resolver.vendorPreview[source] = MaterializedPackage{Root: root, Manifest: value}
}

// ClearVendorPreviews removes operation-scoped migration materializations.
func (resolver *Resolver) ClearVendorPreviews() {
	resolver.previewMu.Lock()
	defer resolver.previewMu.Unlock()
	clear(resolver.vendorPreview)
}

// LatestCommit resolves only release metadata and commit identity. It does not
// download content or modify project state, which keeps outdated read-only.
func (resolver *Resolver) LatestCommit(ctx context.Context, source string) (Release, string, error) {
	repository, err := ParseSource(source)
	if err != nil {
		return Release{}, "", err
	}
	release, err := resolver.LatestRelease(ctx, source)
	if err != nil {
		return Release{}, "", err
	}
	commit, err := resolver.github.ResolveCommit(ctx, repository, release.Tag)
	if err != nil {
		return Release{}, "", err
	}
	return release, commit, nil
}

// TagMatchesVersion reports whether tag names version with at most one
// optional leading v. Publishers use the same rule as release resolution.
func TagMatchesVersion(tag, version string) bool {
	return strings.TrimPrefix(tag, "v") == version
}
