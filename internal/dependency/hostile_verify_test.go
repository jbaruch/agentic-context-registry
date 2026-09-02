package dependency

import (
	"context"
	"strings"
	"testing"
)

func TestHostileInstallVerifiesMetadataAtTagAndIgnoresItAtSHA(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	tag := "v1.2.3"
	archive := packageArchive(t, "1.2.3", "verified\n")

	t.Run("tagMatchingMetadata", func(t *testing.T) {
		t.Parallel()
		remote := &fakeGitHub{
			releases: map[string]Release{tag: {ID: 5, Tag: tag}},
			commits:  map[string]string{tag: commit},
			archives: map[string][]byte{commit: archive},
		}
		resolver := NewResolver(remote)
		baseline, err := resolver.Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
		if err != nil {
			t.Fatal(err)
		}
		remote.releases[tag] = Release{ID: 5, Tag: tag, Assets: []ReleaseAsset{{ID: 9, Name: "acr-package.json"}}}
		remote.assets = map[int64][]byte{9: releaseMetadataJSON(1, commit, baseline.ContentHash)}
		locked, err := resolver.Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
		if err != nil {
			t.Fatal(err)
		}
		if locked.ContentHash != baseline.ContentHash || locked.PackageVersion != "1.2.3" || remote.assetCalls != 1 {
			t.Fatalf("tag install = %#v, asset calls = %d", locked, remote.assetCalls)
		}
		if remote.downloadCalls < 2 {
			t.Fatalf("tag install skipped the source-tree archive: download %d", remote.downloadCalls)
		}
	})

	t.Run("tagCommitMismatch", func(t *testing.T) {
		t.Parallel()
		remote := &fakeGitHub{
			releases: map[string]Release{tag: {ID: 6, Tag: tag, Assets: []ReleaseAsset{{ID: 10, Name: "acr-package.json"}}}},
			commits:  map[string]string{tag: commit},
			archives: map[string][]byte{commit: archive},
			assets:   map[int64][]byte{10: releaseMetadataJSON(1, strings.Repeat("b", 40), "sha256:"+strings.Repeat("0", 64))},
		}
		_, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
		if err == nil || !strings.Contains(err.Error(), "metadata commit mismatch") || !strings.Contains(err.Error(), "do not use") {
			t.Fatalf("Resolve() error = %v", err)
		}
	})

	t.Run("tagContentHashMismatch", func(t *testing.T) {
		t.Parallel()
		remote := &fakeGitHub{
			releases: map[string]Release{tag: {ID: 7, Tag: tag, Assets: []ReleaseAsset{{ID: 11, Name: "acr-package.json"}}}},
			commits:  map[string]string{tag: commit},
			archives: map[string][]byte{commit: archive},
			assets:   map[int64][]byte{11: releaseMetadataJSON(1, commit, "sha256:"+strings.Repeat("0", 64))},
		}
		_, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
		if err == nil || !strings.Contains(err.Error(), "metadata content hash mismatch") || !strings.Contains(err.Error(), "do not use") {
			t.Fatalf("Resolve() error = %v", err)
		}
	})

	t.Run("shaIgnoresTamperedMetadata", func(t *testing.T) {
		t.Parallel()
		remote := &fakeGitHub{
			releases: map[string]Release{tag: {ID: 8, Tag: tag, Assets: []ReleaseAsset{{ID: 12, Name: "acr-package.json"}}}},
			commits:  map[string]string{commit: commit},
			archives: map[string][]byte{commit: archive},
			assets:   map[int64][]byte{12: releaseMetadataJSON(1, strings.Repeat("b", 40), "sha256:"+strings.Repeat("0", 64))},
		}
		locked, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: commit})
		if err != nil {
			t.Fatal(err)
		}
		if locked.Kind != ResolutionCommit || locked.Commit != commit || locked.PackageVersion != "1.2.3" {
			t.Fatalf("SHA install = %#v", locked)
		}
		if remote.releaseCalls != 0 || remote.latestCalls != 0 || remote.assetCalls != 0 {
			t.Fatalf("SHA install consulted release metadata: latest %d exact %d assets %d", remote.latestCalls, remote.releaseCalls, remote.assetCalls)
		}
	})
}
