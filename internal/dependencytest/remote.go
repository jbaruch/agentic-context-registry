// Package dependencytest provides deterministic GitHub doubles for tests that
// exercise the complete application stack.
package dependencytest

import (
	"context"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// Remote is an in-memory dependency.Remote. Map keys use canonical source
// strings, optionally followed by @tag or @commit where the method needs one.
type Remote struct {
	Latest          map[string]dependency.Release
	Releases        map[string]dependency.Release
	Commits         map[string]string
	Archives        map[string][]byte
	Assets          map[int64][]byte
	Existing        map[string]dependency.Release
	TagCommits      map[string]string
	NextReleaseID   int64
	NextAssetID     int64
	CreatedReleases []dependency.Release
	DeletedReleases []int64
}

// NewRemote returns an initialized fake remote.
func NewRemote() *Remote {
	return &Remote{
		Latest:        map[string]dependency.Release{},
		Releases:      map[string]dependency.Release{},
		Commits:       map[string]string{},
		Archives:      map[string][]byte{},
		Assets:        map[int64][]byte{},
		Existing:      map[string]dependency.Release{},
		TagCommits:    map[string]string{},
		NextReleaseID: 1000,
		NextAssetID:   2000,
	}
}

func (remote *Remote) LatestRelease(_ context.Context, repository dependency.Repository) (dependency.Release, error) {
	release, ok := remote.Latest[repository.String()]
	if !ok {
		return dependency.Release{}, fmt.Errorf("no stable release for %s", repository.String())
	}
	return release, nil
}

func (remote *Remote) ReleaseByTag(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, error) {
	release, ok := remote.Releases[repository.String()+"@"+tag]
	if !ok {
		return dependency.Release{}, fmt.Errorf("release %s@%s not found", repository.String(), tag)
	}
	return release, nil
}

func (remote *Remote) ResolveCommit(_ context.Context, repository dependency.Repository, reference string) (string, error) {
	commit, ok := remote.Commits[repository.String()+"@"+reference]
	if !ok {
		return "", fmt.Errorf("commit %s@%s not found", repository.String(), reference)
	}
	return commit, nil
}

func (remote *Remote) DownloadArchive(_ context.Context, repository dependency.Repository, commit string) ([]byte, error) {
	archive, ok := remote.Archives[repository.String()+"@"+commit]
	if !ok {
		return nil, fmt.Errorf("archive %s@%s not found", repository.String(), commit)
	}
	return append([]byte(nil), archive...), nil
}

func (remote *Remote) DownloadReleaseAsset(_ context.Context, _ dependency.Repository, asset dependency.ReleaseAsset) ([]byte, error) {
	contents, ok := remote.Assets[asset.ID]
	if !ok {
		return nil, fmt.Errorf("release asset %d not found", asset.ID)
	}
	return append([]byte(nil), contents...), nil
}

func (remote *Remote) LookupRelease(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, bool, error) {
	release, ok := remote.Existing[repository.String()+"@"+tag]
	return release, ok, nil
}

func (remote *Remote) TagCommit(_ context.Context, repository dependency.Repository, tag string) (string, bool, error) {
	commit, ok := remote.TagCommits[repository.String()+"@"+tag]
	return commit, ok, nil
}

func (remote *Remote) CreateRelease(_ context.Context, repository dependency.Repository, tag, commit string) (dependency.Release, error) {
	remote.NextReleaseID++
	release := dependency.Release{ID: remote.NextReleaseID, Tag: tag, Target: commit, Draft: true}
	remote.Existing[repository.String()+"@"+tag] = release
	remote.CreatedReleases = append(remote.CreatedReleases, release)
	return release, nil
}

func (remote *Remote) UploadAsset(_ context.Context, _ dependency.Repository, releaseID int64, name, _ string, contents []byte) (dependency.ReleaseAsset, []byte, error) {
	remote.NextAssetID++
	asset := dependency.ReleaseAsset{ID: remote.NextAssetID, Name: name}
	remote.Assets[asset.ID] = append([]byte(nil), contents...)
	return asset, append([]byte(nil), contents...), nil
}

func (remote *Remote) PublishRelease(_ context.Context, _ dependency.Repository, releaseID int64) (dependency.Release, error) {
	for _, release := range remote.CreatedReleases {
		if release.ID == releaseID {
			release.Draft = false
			return release, nil
		}
	}
	return dependency.Release{}, fmt.Errorf("draft release %d not found", releaseID)
}

func (remote *Remote) DeleteRelease(_ context.Context, _ dependency.Repository, releaseID int64) error {
	remote.DeletedReleases = append(remote.DeletedReleases, releaseID)
	return nil
}
