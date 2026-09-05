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
// Latest, Releases, Commits and Archives are seeds a test writes before running
// a command. Existing is the live release index every publication method
// mutates. Every lookup resolves through one view of the two: the live index
// answers first, a seed answers when the live index holds nothing for that
// release, and a release this double deleted answers nowhere — so one Remote
// can never call a release absent for a publisher and present for a consumer.
// A release published through the double advances what LatestRelease returns
// even when an older Latest seed is still in place.
//
// CreatedReleases and DeletedReleases are append-only event logs recording what
// a test asked for; no lookup consults them.
type Remote struct {
	Latest     map[string]dependency.Release
	Releases   map[string]dependency.Release
	Commits    map[string]string
	Archives   map[string][]byte
	Assets     map[int64][]byte
	Existing   map[string]dependency.Release
	TagCommits map[string]string
	// NextReleaseID and NextAssetID are allocation floors, not the whole
	// answer. Every allocation first advances past the highest identifier this
	// double can see anywhere — live index, every seed, the assets those
	// releases carry, and the tombstones — so a generated identifier never
	// collides with one a caller seeded, however high that seed is.
	NextReleaseID   int64
	NextAssetID     int64
	CreatedReleases []dependency.Release
	DeletedReleases []int64

	// deleted tombstones the releases this double removed. A seed a caller
	// wrote before the deletion stays where it is, and the tombstone is what
	// stops it answering, so deleting a release never means deleting a test's
	// setup out from under it.
	deleted map[int64]bool
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
		deleted:       map[int64]bool{},
	}
}

// copyRelease detaches a stored release from its caller. Sharing the Assets
// backing array would let a caller that appends to or edits the returned slice
// corrupt the live index.
func copyRelease(release dependency.Release) dependency.Release {
	release.Assets = append([]dependency.ReleaseAsset(nil), release.Assets...)
	return release
}

// find returns the key and release for one release ID, live index first and
// seeds after, so a publication method reaches a release however it got here.
// The caller writes the mutated release back into Existing, which promotes a
// seeded release into the live index the first time anything changes it.
func (remote *Remote) find(releaseID int64) (string, dependency.Release, bool) {
	if remote.deleted[releaseID] {
		return "", dependency.Release{}, false
	}
	for _, index := range []map[string]dependency.Release{remote.Existing, remote.Releases} {
		keys := make([]string, 0, len(index))
		for key := range index {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if index[key].ID == releaseID {
				return key, index[key], true
			}
		}
	}
	sources := make([]string, 0, len(remote.Latest))
	for source := range remote.Latest {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		if release := remote.Latest[source]; release.ID == releaseID {
			return source + "@" + release.Tag, release, true
		}
	}
	return "", dependency.Release{}, false
}

// allocateReleaseID returns an identifier no release this double can see is
// using, and no release it has already deleted ever used. Reusing a seeded
// identifier silently corrupts state: the new release shadows the seeded one in
// every lookup keyed by ID, and deleting the new one tombstones the old one
// too.
func (remote *Remote) allocateReleaseID() int64 {
	highest := remote.NextReleaseID
	for _, index := range []map[string]dependency.Release{remote.Existing, remote.Releases, remote.Latest} {
		for _, release := range index {
			highest = max(highest, release.ID)
		}
	}
	// The event log and the tombstones keep a deleted identifier reserved, so a
	// later allocation cannot resurrect one a lookup is still told to refuse.
	for _, release := range remote.CreatedReleases {
		highest = max(highest, release.ID)
	}
	for _, releaseID := range remote.DeletedReleases {
		highest = max(highest, releaseID)
	}
	for releaseID := range remote.deleted {
		highest = max(highest, releaseID)
	}
	remote.NextReleaseID = highest + 1
	return remote.NextReleaseID
}

// allocateAssetID is the same reservation one level down. An asset identifier
// reused from a seed would rewrite the bytes of an asset nobody uploaded to,
// which is the one thing a published asset must never do.
func (remote *Remote) allocateAssetID() int64 {
	highest := remote.NextAssetID
	for assetID := range remote.Assets {
		highest = max(highest, assetID)
	}
	for _, index := range []map[string]dependency.Release{remote.Existing, remote.Releases, remote.Latest} {
		for _, release := range index {
			for _, asset := range release.Assets {
				highest = max(highest, asset.ID)
			}
		}
	}
	for _, release := range remote.CreatedReleases {
		for _, asset := range release.Assets {
			highest = max(highest, asset.ID)
		}
	}
	remote.NextAssetID = highest + 1
	return remote.NextAssetID
}

// resolve returns the one release this double holds for a source and tag,
// wherever it came from. The live index wins over a seed, and a deleted release
// answers from neither.
func (remote *Remote) resolve(source, tag string) (dependency.Release, bool) {
	key := source + "@" + tag
	if release, ok := remote.Existing[key]; ok && !remote.deleted[release.ID] {
		return release, true
	}
	if release, ok := remote.Releases[key]; ok && !remote.deleted[release.ID] {
		return release, true
	}
	if release, ok := remote.Latest[source]; ok && release.Tag == tag && !remote.deleted[release.ID] {
		return release, true
	}
	return dependency.Release{}, false
}

// consumable reports the release a consumer can resolve for one tag: present,
// not a draft, not a prerelease, and carrying the tag it was asked for.
func (remote *Remote) consumable(source, tag string) (dependency.Release, bool) {
	release, ok := remote.resolve(source, tag)
	if !ok || release.Draft || release.Prerelease || release.Tag != tag {
		return dependency.Release{}, false
	}
	return copyRelease(release), true
}

// newestPublished returns the newest consumable release the live index holds
// for one source. It is what lets a release published through this double
// become the latest one without a second seed.
func (remote *Remote) newestPublished(source string) (dependency.Release, bool) {
	newest := dependency.Release{}
	found := false
	for key, release := range remote.Existing {
		if release.Draft || release.Prerelease || remote.deleted[release.ID] || key != source+"@"+release.Tag {
			continue
		}
		if !found || release.ID >= newest.ID {
			newest, found = release, true
		}
	}
	return newest, found
}

// LatestRelease answers from the newest release this double actually holds: a
// release published through it wins over an older Latest seed, a deleted one
// answers not at all, and the seed still answers when nothing was published.
func (remote *Remote) LatestRelease(_ context.Context, repository dependency.Repository) (dependency.Release, error) {
	source := repository.String()
	seeded, hasSeed := remote.Latest[source]
	if hasSeed && remote.deleted[seeded.ID] {
		hasSeed = false
	}
	published, hasPublished := remote.newestPublished(source)
	switch {
	case hasPublished && (!hasSeed || published.ID >= seeded.ID):
		return copyRelease(published), nil
	case hasSeed:
		return copyRelease(seeded), nil
	}
	return dependency.Release{}, fmt.Errorf("no stable release for %s", source)
}

func (remote *Remote) ReleaseByTag(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, error) {
	if release, ok := remote.consumable(repository.String(), tag); ok {
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

// LookupRelease is the publisher's own preflight. It reads the same view a
// consumer reads, so a release one of them can see is a release both can see.
func (remote *Remote) LookupRelease(_ context.Context, repository dependency.Repository, tag string) (dependency.Release, bool, error) {
	release, ok := remote.resolve(repository.String(), tag)
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
	if _, exists := remote.resolve(repository.String(), tag); exists {
		return dependency.Release{}, fmt.Errorf("release %s already exists", key)
	}
	release := dependency.Release{ID: remote.allocateReleaseID(), Tag: tag, Target: commit, Draft: true}
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
	assetID := remote.allocateAssetID()
	asset := dependency.ReleaseAsset{
		ID:   assetID,
		Name: name,
		URL:  fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", key, assetID),
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
	if remote.deleted == nil {
		remote.deleted = map[int64]bool{}
	}
	remote.deleted[releaseID] = true
	remote.DeletedReleases = append(remote.DeletedReleases, releaseID)
	return nil
}
