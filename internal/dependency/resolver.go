package dependency

import (
	"context"
	"fmt"
	"strings"
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

	locked := LockedDependency{Source: declaration.Source, Requested: declaration.Requested}
	reference := declaration.Requested
	switch {
	case declaration.Requested == "latest":
		release, err := resolver.github.LatestRelease(ctx, repository)
		if err != nil {
			return LockedDependency{}, err
		}
		locked.Kind = ResolutionRelease
		locked.ReleaseID = release.ID
		locked.Tag = release.Tag
		reference = release.Tag
	case isCommitRequest(declaration.Requested):
		locked.Kind = ResolutionCommit
	default:
		release, err := resolver.github.ReleaseByTag(ctx, repository, declaration.Requested)
		if err != nil {
			return LockedDependency{}, err
		}
		locked.Kind = ResolutionRelease
		locked.ReleaseID = release.ID
		locked.Tag = release.Tag
		reference = release.Tag
	}

	locked.Commit, err = resolver.github.ResolveCommit(ctx, repository, reference)
	if err != nil {
		return LockedDependency{}, err
	}
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
	repository, err := ParseSource(locked.Source)
	if err != nil {
		return err
	}
	if len(locked.Commit) != 40 || !isCommitRequest(locked.Commit) {
		return fmt.Errorf("locked commit %q for %s is invalid; run 'acr install' to regenerate %s", locked.Commit, locked.Source, LockFilename)
	}
	archive, err := resolver.github.DownloadArchive(ctx, repository, locked.Commit)
	if err != nil {
		return err
	}
	verified, err := verifyPackageArchive(archive, repository)
	if err != nil {
		return err
	}
	if verified.Version != locked.PackageVersion {
		return fmt.Errorf("locked package version mismatch for %s: expected %s, downloaded %s; remove %s and run 'acr install'", locked.Source, locked.PackageVersion, verified.Version, LockFilename)
	}
	if verified.ContentHash != locked.ContentHash {
		return fmt.Errorf("content hash mismatch for %s at %s: expected %s, downloaded %s; do not use the package and verify the repository contents", locked.Source, locked.Commit, locked.ContentHash, verified.ContentHash)
	}
	return nil
}

// LatestCommit resolves only release metadata and commit identity. It does not
// download content or modify project state, which keeps outdated read-only.
func (resolver *Resolver) LatestCommit(ctx context.Context, source string) (Release, string, error) {
	repository, err := ParseSource(source)
	if err != nil {
		return Release{}, "", err
	}
	release, err := resolver.github.LatestRelease(ctx, repository)
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
