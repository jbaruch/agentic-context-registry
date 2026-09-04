// Package dependencytest provides deterministic GitHub doubles for tests that
// exercise the complete application stack.
package dependencytest

import (
	"context"
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// Remote is an in-memory dependency.Remote. Map keys use canonical source
// strings, optionally followed by @tag or @commit where the method needs one.
//
// Resolution inputs (Latest, Releases, Commits, Archives) are seeds a test
// writes before running a command. Existing is the live release index: it is a
// seed too, and it is also the one representation the publication methods
// mutate, so a release published, uploaded to, or deleted through this double
// is visible to every later lookup exactly as GitHub would show it.
// CreatedReleases and DeletedReleases are append-only event logs recording
// what a test asked for; they are never consulted by a lookup.
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

// copyRelease detaches a stored release from its caller. Sharing the Assets
// backing array would let a caller that appends to or edits the returned slice
// corrupt the live index.
func copyRelease(release dependency.Release) dependency.Release {
	release.Assets = append([]dependency.ReleaseAsset(nil), release.Assets...)
	return release
}

// find returns the live key and release for one release ID.
func (remote *Remote) find(releaseID int64) (string, dependency.Release, bool) {
	keys := make([]string, 0, len(remote.Existing))
	for key := range remote.Existing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if remote.Existing[key].ID == releaseID {
			return key, remote.Existing[key], true
		}
	}
	return "", dependency.Release{}, false
}

// published returns the live release for one source and tag when it is
// consumable: present, not a draft, and not a prerelease.
func (remote *Remote) published(source, tag string) (dependency.Release, bool) {
	release, ok := remote.Existing[source+"@"+tag]
	if !ok || release.Draft || release.Prerelease || release.Tag != tag {
		return dependency.Release{}, false
	}
	return copyRelease(release), true
}

func (remote *Remote) LatestRelease(_ context.Context, repository dependency.Repository) (dependency.Release, error) {
	source := repository.String()
	if release, ok := remote.Latest[source]; ok {
		return copyRelease(release), nil
	}
	// A release this double published is discoverable without a second seed,
	// so a publisher-to-consumer journey never fabricates its own outcome.
	newest := dependency.Release{}
	for key := range remote.Existing {
		release := remote.Existing[key]
		if release.Draft || release.Prerelease || key != source+"@"+release.Tag {
			continue
		}
		if release.ID > newest.ID {
			newest = release
		}
	}
	if newest.ID > 0 {
		return copyRelease(newest), nil
	}
	return dependency.Release{}, fmt.Errorf("no stable release for %s", source)
}

func (remote *Remote) ReleaseByTag(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, error) {
	if release, ok := remote.Releases[repository.String()+"@"+tag]; ok {
		return copyRelease(release), nil
	}
	if release, ok := remote.published(repository.String(), tag); ok {
		return release, nil
	}
	return dependency.Release{}, fmt.Errorf("release %s@%s not found", repository.String(), tag)
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
	if !ok {
		return dependency.Release{}, false, nil
	}
	return copyRelease(release), true, nil
}

func (remote *Remote) TagCommit(_ context.Context, repository dependency.Repository, tag string) (string, bool, error) {
	commit, ok := remote.TagCommits[repository.String()+"@"+tag]
	return commit, ok, nil
}

func (remote *Remote) CreateRelease(_ context.Context, repository dependency.Repository, tag, commit string) (dependency.Release, error) {
	key := repository.String() + "@" + tag
	if _, exists := remote.Existing[key]; exists {
		return dependency.Release{}, fmt.Errorf("release %s already exists", key)
	}
	remote.NextReleaseID++
	release := dependency.Release{ID: remote.NextReleaseID, Tag: tag, Target: commit, Draft: true}
	remote.Existing[key] = release
	remote.CreatedReleases = append(remote.CreatedReleases, copyRelease(release))
	return copyRelease(release), nil
}

func (remote *Remote) UploadAsset(_ context.Context, _ dependency.Repository, releaseID int64, name, _ string, contents []byte) (dependency.ReleaseAsset, []byte, error) {
	key, release, ok := remote.find(releaseID)
	if !ok {
		return dependency.ReleaseAsset{}, nil, fmt.Errorf("release %d not found", releaseID)
	}
	for _, existing := range release.Assets {
		if existing.Name == name {
			return dependency.ReleaseAsset{}, nil, fmt.Errorf("release %d already has an asset named %q", releaseID, name)
		}
	}
	remote.NextAssetID++
	asset := dependency.ReleaseAsset{
		ID:   remote.NextAssetID,
		Name: name,
		URL:  fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", key, remote.NextAssetID),
	}
	remote.Assets[asset.ID] = append([]byte(nil), contents...)
	release.Assets = append(append([]dependency.ReleaseAsset(nil), release.Assets...), asset)
	remote.Existing[key] = release
	return asset, append([]byte(nil), contents...), nil
}

func (remote *Remote) PublishRelease(_ context.Context, _ dependency.Repository, releaseID int64) (dependency.Release, error) {
	key, release, ok := remote.find(releaseID)
	if !ok {
		return dependency.Release{}, fmt.Errorf("draft release %d not found", releaseID)
	}
	release.Draft = false
	remote.Existing[key] = release
	return copyRelease(release), nil
}

func (remote *Remote) DeleteRelease(_ context.Context, _ dependency.Repository, releaseID int64) error {
	key, _, ok := remote.find(releaseID)
	if !ok {
		return fmt.Errorf("release %d not found", releaseID)
	}
	delete(remote.Existing, key)
	remote.DeletedReleases = append(remote.DeletedReleases, releaseID)
	return nil
}
