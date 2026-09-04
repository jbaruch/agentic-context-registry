package dependencytest

import (
	"context"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

var testRepository = dependency.Repository{Owner: "owner", Name: "package"}

func TestRemoteCopiesArchiveBytes(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	repository := testRepository
	key := repository.String() + "@" + "0123456789abcdef0123456789abcdef01234567"
	remote.Archives[key] = []byte("archive")

	first, err := remote.DownloadArchive(context.Background(), repository, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'A'
	second, err := remote.DownloadArchive(context.Background(), repository, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(second), "archive"; got != want {
		t.Fatalf("DownloadArchive() = %q, want %q", got, want)
	}
}

// createDraft is the publisher's own first write. Nothing here seeds a release
// state a later assertion then reads back.
func createDraft(t *testing.T, remote *Remote, tag string) dependency.Release {
	t.Helper()
	draft, err := remote.CreateRelease(context.Background(), testRepository, tag, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if !draft.Draft || draft.ID <= 0 || draft.Tag != tag {
		t.Fatalf("CreateRelease() = %#v, want a draft carrying tag %q", draft, tag)
	}
	return draft
}

func TestRemotePublishThenLookup(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")

	published, err := remote.PublishRelease(context.Background(), testRepository, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Draft || published.ID != draft.ID {
		t.Fatalf("PublishRelease() = %#v, want the same release published", published)
	}
	looked, exists, err := remote.LookupRelease(context.Background(), testRepository, "v1.0.0")
	if err != nil || !exists {
		t.Fatalf("LookupRelease() = %#v, %t, %v", looked, exists, err)
	}
	if looked.Draft {
		t.Fatalf("LookupRelease() = %#v, want the published release, not a stale draft", looked)
	}
}

func TestRemoteDeleteThenLookup(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")

	if err := remote.DeleteRelease(context.Background(), testRepository, draft.ID); err != nil {
		t.Fatal(err)
	}
	_, exists, err := remote.LookupRelease(context.Background(), testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("LookupRelease() still sees a deleted release")
	}
	if len(remote.DeletedReleases) != 1 || remote.DeletedReleases[0] != draft.ID {
		t.Fatalf("DeletedReleases = %v, want the deletion log to keep %d", remote.DeletedReleases, draft.ID)
	}
	// The tag is free again, which is what makes a clean retry possible.
	createDraft(t, remote, "v1.0.0")
}

func TestRemoteUploadMembership(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")

	asset, verified, err := remote.UploadAsset(context.Background(), testRepository, draft.ID, dependency.ReleaseMetadataAssetName, "application/json", []byte(`{"metadataVersion":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if asset.ID <= 0 || asset.Name != dependency.ReleaseMetadataAssetName || asset.URL == "" {
		t.Fatalf("UploadAsset() = %#v, want complete asset metadata", asset)
	}
	if string(verified) != `{"metadataVersion":1}` {
		t.Fatalf("UploadAsset() readback = %q", verified)
	}
	looked, exists, err := remote.LookupRelease(context.Background(), testRepository, "v1.0.0")
	if err != nil || !exists {
		t.Fatalf("LookupRelease() = %#v, %t, %v", looked, exists, err)
	}
	if len(looked.Assets) != 1 || looked.Assets[0].ID != asset.ID {
		t.Fatalf("release assets = %#v, want the uploaded asset to belong to the release", looked.Assets)
	}
	contents, err := remote.DownloadReleaseAsset(context.Background(), testRepository, looked.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != `{"metadataVersion":1}` {
		t.Fatalf("DownloadReleaseAsset() = %q", contents)
	}
	if _, _, err := remote.UploadAsset(context.Background(), testRepository, draft.ID+7, "extra.txt", "text/plain", nil); err == nil {
		t.Fatal("UploadAsset() accepted an unknown release ID")
	}
}

func TestRemotePublishedReleaseIsConsumable(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")
	if _, _, err := remote.UploadAsset(context.Background(), testRepository, draft.ID, dependency.ReleaseMetadataAssetName, "application/json", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	// A draft is not consumable, and neither LatestRelease nor ReleaseByTag
	// invents one before publication.
	if release, err := remote.LatestRelease(context.Background(), testRepository); err == nil {
		t.Fatalf("LatestRelease() = %#v, want a draft to stay invisible", release)
	}
	if release, err := remote.ReleaseByTag(context.Background(), testRepository, "v1.0.0"); err == nil {
		t.Fatalf("ReleaseByTag() = %#v, want a draft to stay invisible", release)
	}

	if _, err := remote.PublishRelease(context.Background(), testRepository, draft.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := remote.LatestRelease(context.Background(), testRepository)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != draft.ID || latest.Tag != "v1.0.0" || len(latest.Assets) != 1 {
		t.Fatalf("LatestRelease() = %#v, want the published release with its asset", latest)
	}
	byTag, err := remote.ReleaseByTag(context.Background(), testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if byTag.ID != draft.ID {
		t.Fatalf("ReleaseByTag() = %#v, want release %d", byTag, draft.ID)
	}

	// Mutating what a lookup returned must not reach the stored release.
	byTag.Assets[0].Name = "tampered"
	byTag.Assets = append(byTag.Assets, dependency.ReleaseAsset{ID: 9, Name: "extra"})
	again, err := remote.ReleaseByTag(context.Background(), testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Assets) != 1 || again.Assets[0].Name != dependency.ReleaseMetadataAssetName {
		t.Fatalf("stored assets = %#v, want them unaffected by a caller's edits", again.Assets)
	}
}

var _ dependency.Remote = (*Remote)(nil)
