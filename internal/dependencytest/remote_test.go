package dependencytest

import (
	"context"
	"fmt"
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

// TestRemoteSeededViewsFollowADeletion is the review's counterexample: both
// consumer views are seeded with a release that is then deleted through the
// publication API. A seed that outlived the release it describes would let one
// Remote call the release absent for the publisher and present for a consumer.
func TestRemoteSeededViewsFollowADeletion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")
	published, err := remote.PublishRelease(ctx, testRepository, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	remote.Latest[testRepository.String()] = published
	remote.Releases[testRepository.String()+"@v1.0.0"] = published

	if _, err := remote.LatestRelease(ctx, testRepository); err != nil {
		t.Fatalf("a seeded published release is not visible: %v", err)
	}

	if err := remote.DeleteRelease(ctx, testRepository, draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := remote.LookupRelease(ctx, testRepository, "v1.0.0"); err != nil || exists {
		t.Fatalf("LookupRelease() = %t, %v, want the deleted release gone", exists, err)
	}
	if release, err := remote.LatestRelease(ctx, testRepository); err == nil {
		t.Errorf("LatestRelease() still returns deleted release %d", release.ID)
	}
	if release, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0"); err == nil {
		t.Errorf("ReleaseByTag() still returns deleted release %d", release.ID)
	}

	// The seeds a caller wrote are still theirs; only the tombstone stops them
	// answering, and the event log still records what happened.
	if _, seeded := remote.Latest[testRepository.String()]; !seeded {
		t.Error("the deletion removed a caller's Latest seed")
	}
	if len(remote.CreatedReleases) != 1 || len(remote.DeletedReleases) != 1 {
		t.Fatalf("event logs = %d created, %d deleted, want one each", len(remote.CreatedReleases), len(remote.DeletedReleases))
	}
	if remote.DeletedReleases[0] != draft.ID {
		t.Fatalf("DeletedReleases = %v, want %d", remote.DeletedReleases, draft.ID)
	}
}

// TestRemotePublicationAdvancesASeededLatest proves the other direction: a
// release published through the double is newer than the seed a test wrote
// before it, so a consumer resolving latest sees the publication.
func TestRemotePublicationAdvancesASeededLatest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	remote.Latest[testRepository.String()] = dependency.Release{ID: 7, Tag: "v0.9.0"}
	remote.Releases[testRepository.String()+"@v0.9.0"] = dependency.Release{ID: 7, Tag: "v0.9.0"}

	// Before publication the seed is still the answer.
	if latest, err := remote.LatestRelease(ctx, testRepository); err != nil || latest.Tag != "v0.9.0" {
		t.Fatalf("LatestRelease() = %#v, %v, want the seeded release", latest, err)
	}

	draft := createDraft(t, remote, "v1.0.0")
	// A draft does not advance latest.
	if latest, err := remote.LatestRelease(ctx, testRepository); err != nil || latest.Tag != "v0.9.0" {
		t.Fatalf("LatestRelease() = %#v, %v, want a draft to stay invisible", latest, err)
	}
	if _, err := remote.PublishRelease(ctx, testRepository, draft.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := remote.LatestRelease(ctx, testRepository)
	if err != nil || latest.Tag != "v1.0.0" || latest.ID != draft.ID {
		t.Fatalf("LatestRelease() = %#v, %v, want the published release", latest, err)
	}
	// The older tag still resolves to what it always was.
	if older, err := remote.ReleaseByTag(ctx, testRepository, "v0.9.0"); err != nil || older.ID != 7 {
		t.Fatalf("ReleaseByTag(v0.9.0) = %#v, %v, want the seed to keep answering", older, err)
	}
}

// TestRemoteSeededReleaseAcceptsPublication covers the promotion path: a
// release that only ever existed as a seed can still be uploaded to and
// published, and every view observes the result.
func TestRemoteSeededReleaseAcceptsPublication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	remote.Releases[testRepository.String()+"@v1.0.0"] = dependency.Release{ID: 11, Tag: "v1.0.0", Draft: true}

	// A seeded draft is not consumable, and the tag is not free either.
	if release, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0"); err == nil {
		t.Fatalf("ReleaseByTag() = %#v, want a seeded draft to stay invisible", release)
	}
	if _, exists, err := remote.LookupRelease(ctx, testRepository, "v1.0.0"); err != nil || !exists {
		t.Fatalf("LookupRelease() = %t, %v, want the publisher to see the seeded draft", exists, err)
	}
	if _, err := remote.CreateRelease(ctx, testRepository, "v1.0.0", "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("CreateRelease() overwrote a release a lookup can already see")
	}

	asset, _, err := remote.UploadAsset(ctx, testRepository, 11, "notes.txt", "text/plain", []byte("notes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.PublishRelease(ctx, testRepository, 11); err != nil {
		t.Fatal(err)
	}
	byTag, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(byTag.Assets) != 1 || byTag.Assets[0].ID != asset.ID {
		t.Fatalf("ReleaseByTag() assets = %#v, want the uploaded asset", byTag.Assets)
	}
	latest, err := remote.LatestRelease(ctx, testRepository)
	if err != nil || latest.ID != 11 || len(latest.Assets) != 1 {
		t.Fatalf("LatestRelease() = %#v, %v, want the published seed with its asset", latest, err)
	}
}

// TestRemoteReturnedReleasesDoNotReachStorage holds every read to the same
// promise: what a caller does with a returned release stays with the caller.
func TestRemoteReturnedReleasesDoNotReachStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	draft := createDraft(t, remote, "v1.0.0")
	if _, _, err := remote.UploadAsset(ctx, testRepository, draft.ID, "notes.txt", "text/plain", []byte("notes")); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.PublishRelease(ctx, testRepository, draft.ID); err != nil {
		t.Fatal(err)
	}

	tamper := func(release dependency.Release) {
		release.Tag = "tampered"
		release.Draft = true
		if len(release.Assets) != 0 {
			release.Assets[0].Name = "tampered"
			release.Assets[0].ID = 0
		}
	}
	latest, err := remote.LatestRelease(ctx, testRepository)
	if err != nil {
		t.Fatal(err)
	}
	tamper(latest)
	byTag, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	tamper(byTag)
	looked, exists, err := remote.LookupRelease(ctx, testRepository, "v1.0.0")
	if err != nil || !exists {
		t.Fatalf("LookupRelease() = %t, %v", exists, err)
	}
	tamper(looked)

	stored, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Tag != "v1.0.0" || stored.Draft {
		t.Fatalf("stored release = %#v, want it unaffected by a caller's edits", stored)
	}
	if len(stored.Assets) != 1 || stored.Assets[0].Name != "notes.txt" {
		t.Fatalf("stored assets = %#v, want them unaffected by a caller's edits", stored.Assets)
	}
	// The event log records the creation, not the caller's edits.
	if len(remote.CreatedReleases) != 1 || remote.CreatedReleases[0].Tag != "v1.0.0" {
		t.Fatalf("CreatedReleases = %#v, want the creation event intact", remote.CreatedReleases)
	}
}

// TestRemoteReservesSeededReleaseIDs runs the review's controls: a seed below
// the allocation floor, a seed exactly on the first identifier the floor would
// hand out, and a seed far above it. A caller seeds a release and never touches
// an allocation counter, so the double has to reserve what it can already see.
func TestRemoteReservesSeededReleaseIDs(t *testing.T) {
	t.Parallel()

	for _, seededID := range []int64{7, 1001, 5000} {
		seededID := seededID
		t.Run(fmt.Sprint(seededID), func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			remote := NewRemote()
			old := dependency.Release{ID: seededID, Tag: "v1.0.0"}
			remote.Latest[testRepository.String()] = old
			remote.Releases[testRepository.String()+"@v1.0.0"] = old

			draft, err := remote.CreateRelease(ctx, testRepository, "v2.0.0", "0123456789abcdef0123456789abcdef01234567")
			if err != nil {
				t.Fatal(err)
			}
			if draft.ID == seededID {
				t.Fatalf("new release ID %d collides with the preseeded v1.0.0", draft.ID)
			}
			asset, _, err := remote.UploadAsset(ctx, testRepository, draft.ID, "notes.txt", "text/plain", []byte("new notes"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remote.PublishRelease(ctx, testRepository, draft.ID); err != nil {
				t.Fatal(err)
			}

			// A publication is newer than anything seeded before it, whatever
			// identifier the seed happened to carry.
			latest, err := remote.LatestRelease(ctx, testRepository)
			if err != nil {
				t.Fatal(err)
			}
			if latest.Tag != "v2.0.0" || latest.ID != draft.ID {
				t.Fatalf("LatestRelease() = %#v, want the release just published as %d", latest, draft.ID)
			}
			if len(latest.Assets) != 1 || latest.Assets[0].ID != asset.ID {
				t.Fatalf("LatestRelease() assets = %#v, want the uploaded asset %d", latest.Assets, asset.ID)
			}

			// Deleting the new release is isolated from the seeded one.
			if err := remote.DeleteRelease(ctx, testRepository, draft.ID); err != nil {
				t.Fatal(err)
			}
			survivor, err := remote.ReleaseByTag(ctx, testRepository, "v1.0.0")
			if err != nil || survivor.ID != seededID {
				t.Fatalf("ReleaseByTag(v1.0.0) = %#v, %v, want the untouched seed %d", survivor, err, seededID)
			}
			if latest, err := remote.LatestRelease(ctx, testRepository); err != nil || latest.ID != seededID {
				t.Fatalf("LatestRelease() = %#v, %v, want it to fall back to the seed", latest, err)
			}

			// A deleted identifier stays reserved, so the next allocation cannot
			// resurrect a release every lookup is told to refuse.
			reissued, err := remote.CreateRelease(ctx, testRepository, "v3.0.0", "0123456789abcdef0123456789abcdef01234567")
			if err != nil {
				t.Fatal(err)
			}
			if reissued.ID == draft.ID || reissued.ID == seededID {
				t.Fatalf("reissued release ID %d reuses %d or %d", reissued.ID, draft.ID, seededID)
			}
			if _, err := remote.PublishRelease(ctx, testRepository, reissued.ID); err != nil {
				t.Fatal(err)
			}
			if latest, err := remote.LatestRelease(ctx, testRepository); err != nil || latest.Tag != "v3.0.0" {
				t.Fatalf("LatestRelease() = %#v, %v, want the reissued publication", latest, err)
			}
		})
	}
}

// TestRemoteReservesSeededAssetIDs is the same reservation one level down. An
// upload that reused a seeded asset identifier would rewrite the bytes of an
// asset nobody uploaded to, which is the one thing a published asset must never
// do.
func TestRemoteReservesSeededAssetIDs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	old := dependency.ReleaseAsset{ID: 2001, Name: "old.txt"}
	remote.Assets[old.ID] = []byte("old bytes")
	remote.Releases[testRepository.String()+"@v1.0.0"] = dependency.Release{
		ID: 7, Tag: "v1.0.0", Assets: []dependency.ReleaseAsset{old},
	}

	fresh, _, err := remote.UploadAsset(ctx, testRepository, 7, "new.txt", "text/plain", []byte("new bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == old.ID {
		t.Fatalf("upload reused the seeded asset ID %d", old.ID)
	}
	previous, err := remote.DownloadReleaseAsset(ctx, testRepository, old)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "old bytes" {
		t.Fatalf("the seeded asset now reads %q, want its own bytes", previous)
	}
	current, err := remote.DownloadReleaseAsset(ctx, testRepository, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "new bytes" {
		t.Fatalf("the uploaded asset reads %q", current)
	}

	// Both assets belong to the release, and the seeded one keeps its identity.
	release, exists, err := remote.LookupRelease(ctx, testRepository, "v1.0.0")
	if err != nil || !exists {
		t.Fatalf("LookupRelease() = %t, %v", exists, err)
	}
	if len(release.Assets) != 2 {
		t.Fatalf("release assets = %#v, want the seeded and the uploaded asset", release.Assets)
	}
	if release.Assets[0].ID != old.ID || release.Assets[0].Name != old.Name {
		t.Fatalf("seeded asset = %#v, want it unchanged", release.Assets[0])
	}
}

// TestRemoteAllocationSurvivesAHighSeedArrivingLate covers the ordering nobody
// controls: a caller may seed a release after an earlier allocation, and the
// next allocation still has to clear everything now visible.
func TestRemoteAllocationSurvivesAHighSeedArrivingLate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	remote := NewRemote()
	first := createDraft(t, remote, "v1.0.0")

	remote.Releases[testRepository.String()+"@v9.0.0"] = dependency.Release{ID: 90000, Tag: "v9.0.0"}
	second, err := remote.CreateRelease(ctx, testRepository, "v2.0.0", "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID <= 90000 || second.ID == first.ID {
		t.Fatalf("release ID %d does not clear the late seed 90000 and the earlier %d", second.ID, first.ID)
	}
	if _, err := remote.PublishRelease(ctx, testRepository, second.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := remote.LatestRelease(ctx, testRepository)
	if err != nil || latest.ID != second.ID {
		t.Fatalf("LatestRelease() = %#v, %v, want the publication to win over the late seed", latest, err)
	}
}

var _ dependency.Remote = (*Remote)(nil)
