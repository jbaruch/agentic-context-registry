package dependency

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Resolver turns requested policies into immutable verified locks.
type Resolver struct {
	github GitHub
}

// NewResolver constructs a dependency resolver using the supplied GitHub API.
func NewResolver(github GitHub) *Resolver {
	return &Resolver{github: github}
}

// Resolve resolves and verifies one declaration.
func (resolver *Resolver) Resolve(ctx context.Context, declaration Declaration) (LockedDependency, error) {
	repository, err := ParseSource(declaration.Source)
	if err != nil {
		return LockedDependency{}, err
	}
	if err := validateRequested(declaration.Requested); err != nil {
		return LockedDependency{}, fmt.Errorf("invalid requested policy %q for %s: %w", declaration.Requested, declaration.Source, err)
	}

	switch {
	case declaration.Requested == "latest":
		release, err := resolver.LatestRelease(ctx, declaration.Source)
		if err != nil {
			return LockedDependency{}, err
		}
		return resolver.resolveLatestCandidate(ctx, declaration, release)
	case isCommitRequest(declaration.Requested):
		commit, err := resolver.github.ResolveCommit(ctx, repository, declaration.Requested)
		if err != nil {
			return LockedDependency{}, err
		}
		locked := LockedDependency{Source: declaration.Source, Requested: declaration.Requested, Kind: ResolutionCommit, Commit: commit}
		return resolver.finishResolution(ctx, repository, locked)
	default:
		release, err := resolver.github.ReleaseByTag(ctx, repository, declaration.Requested)
		if err != nil {
			return LockedDependency{}, err
		}
		commit, err := resolver.github.ResolveCommit(ctx, repository, release.Tag)
		if err != nil {
			return LockedDependency{}, err
		}
		locked := LockedDependency{
			Source: declaration.Source, Requested: declaration.Requested, Kind: ResolutionRelease,
			ReleaseID: release.ID, Tag: release.Tag, Commit: commit,
		}
		return resolver.finishResolution(ctx, repository, locked)
	}
}

func (resolver *Resolver) resolveLatestCandidate(ctx context.Context, declaration Declaration, release Release) (LockedDependency, error) {
	if release.ID <= 0 || release.Tag == "" || release.Draft || release.Prerelease {
		return LockedDependency{}, fmt.Errorf("latest release candidate for %s is not stable; publish a stable GitHub Release and retry", declaration.Source)
	}
	repository, err := ParseSource(declaration.Source)
	if err != nil {
		return LockedDependency{}, err
	}
	commit, err := resolver.github.ResolveCommit(ctx, repository, release.Tag)
	if err != nil {
		return LockedDependency{}, err
	}
	locked := LockedDependency{
		Source: declaration.Source, Requested: declaration.Requested, Kind: ResolutionRelease,
		ReleaseID: release.ID, Tag: release.Tag, Commit: commit,
	}
	return resolver.finishResolution(ctx, repository, locked)
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
	if locked.Kind == ResolutionRelease && !tagMatchesVersion(locked.Tag, verified.Version) {
		return LockedDependency{}, fmt.Errorf("release tag %q does not match package version %q; publish matching agent-plugin.yaml metadata and retry", locked.Tag, verified.Version)
	}
	locked.PackageVersion = verified.Version
	locked.ContentHash = verified.ContentHash
	return locked, nil
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
	if err = extractGitHubArchive(archive, root); err != nil {
		return MaterializedPackage{}, nil, err
	}
	value, err := manifest.Load(root)
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("validate downloaded %s package: %w; fix the package manifest and publish a new release", repository.String(), err)
	}
	if value.Name != repository.FullName() || value.Source.Repository != "https://github.com/"+repository.FullName() {
		return MaterializedPackage{}, nil, fmt.Errorf("downloaded package identity %q does not match %s; fix agent-plugin.yaml and publish a new release", value.Name, repository.String())
	}
	contentHash, err := hashPackageFiles(root, value)
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

func tagMatchesVersion(tag, version string) bool {
	return strings.TrimPrefix(tag, "v") == version
}
