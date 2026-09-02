package dependency

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestInstallFromTagWithoutReleaseAssets(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 42, Tag: "v1.2.3"},
		commits:  map[string]string{"v1.2.3": commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.2.3", "content\n")},
	}
	resolver := NewResolver(remote)

	locked, err := resolver.Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: "latest"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if locked.Kind != ResolutionRelease || locked.ReleaseID != 42 || locked.Tag != "v1.2.3" || locked.Commit != commit || locked.PackageVersion != "1.2.3" || !strings.HasPrefix(locked.ContentHash, "sha256:") {
		t.Fatalf("Resolve() = %#v", locked)
	}
	if remote.latestCalls != 1 || remote.resolveCalls != 1 || remote.downloadCalls != 1 {
		t.Fatalf("remote calls = latest %d, resolve %d, download %d", remote.latestCalls, remote.resolveCalls, remote.downloadCalls)
	}
	if err := resolver.VerifyLocked(context.Background(), locked); err != nil {
		t.Fatalf("VerifyLocked() error = %v", err)
	}
	if remote.latestCalls != 1 || remote.resolveCalls != 1 || remote.downloadCalls != 2 {
		t.Fatalf("VerifyLocked resolved mutable metadata: latest %d, resolve %d, download %d", remote.latestCalls, remote.resolveCalls, remote.downloadCalls)
	}
}

func TestResolverResolvesAtACallerSuppliedCandidate(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	declaration := Declaration{Source: "github:owner/plugin", Requested: "latest"}
	remote := &fakeGitHub{
		latest:   Release{ID: 42, Tag: "v1.2.3"},
		commits:  map[string]string{"v1.2.3": commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.2.3", "content\n")},
	}
	resolver := NewResolver(remote)

	release, err := resolver.Candidate(context.Background(), declaration)
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	locked, err := resolver.ResolveAt(context.Background(), declaration, release)
	if err != nil {
		t.Fatalf("ResolveAt() error = %v", err)
	}
	if locked.ReleaseID != 42 || locked.Tag != "v1.2.3" || locked.Commit != commit {
		t.Fatalf("ResolveAt() = %#v", locked)
	}
	if remote.latestCalls != 1 {
		t.Fatalf("ResolveAt() refetched the candidate: latest calls = %d", remote.latestCalls)
	}
}

func TestResolverCandidateSkipsReleaseMetadataForCommitRequests(t *testing.T) {
	t.Parallel()

	requested := strings.Repeat("b", 12)
	remote := &fakeGitHub{}
	release, err := NewResolver(remote).Candidate(context.Background(), Declaration{Source: "github:owner/plugin", Requested: requested})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if !reflect.DeepEqual(release, Release{}) || remote.latestCalls != 0 || remote.releaseCalls != 0 {
		t.Fatalf("Candidate() = %#v, remote = %#v, want no release lookup", release, remote)
	}
}

func TestResolveAtRejectsUnstableCandidates(t *testing.T) {
	t.Parallel()

	declaration := Declaration{Source: "github:owner/plugin", Requested: "latest"}
	for name, release := range map[string]Release{
		"draft":      {ID: 5, Tag: "v1.4.1", Draft: true},
		"prerelease": {ID: 5, Tag: "v1.4.1-rc.1", Prerelease: true},
		"unreleased": {Tag: "v1.4.1"},
	} {
		t.Run(name, func(t *testing.T) {
			remote := &fakeGitHub{}
			_, err := NewResolver(remote).ResolveAt(context.Background(), declaration, release)
			if err == nil || !strings.Contains(err.Error(), "is not stable") {
				t.Fatalf("ResolveAt() error = %v, want a stability rejection", err)
			}
			if remote.resolveCalls != 0 || remote.downloadCalls != 0 {
				t.Fatalf("ResolveAt() contacted the remote for an unstable candidate: %#v", remote)
			}
		})
	}
}

func TestInstallFromCommitWithoutRelease(t *testing.T) {
	t.Parallel()

	requested := strings.Repeat("b", 12)
	commit := strings.Repeat("b", 40)
	remote := &fakeGitHub{
		commits:  map[string]string{requested: commit},
		archives: map[string][]byte{commit: packageArchive(t, "3.0.0", "pinned\n")},
	}
	locked, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: requested})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if locked.Kind != ResolutionCommit || locked.ReleaseID != 0 || locked.Tag != "" || locked.Commit != commit {
		t.Fatalf("Resolve() = %#v, want commit pin", locked)
	}
	if remote.latestCalls != 0 || remote.releaseCalls != 0 {
		t.Fatalf("commit pin queried releases: latest %d, exact %d", remote.latestCalls, remote.releaseCalls)
	}
	if remote.assetCalls != 0 {
		t.Fatalf("commit pin queried release assets: %d", remote.assetCalls)
	}
}

func TestResolverRejectsReleaseManifestVersionMismatch(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("c", 40)
	remote := &fakeGitHub{
		releases: map[string]Release{"v2.0.0": {ID: 2, Tag: "v2.0.0"}},
		commits:  map[string]string{"v2.0.0": commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "mismatch\n")},
	}
	_, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: "v2.0.0"})
	if err == nil || !strings.Contains(err.Error(), "does not match package version") {
		t.Fatalf("Resolve() error = %v, want version mismatch", err)
	}
}

func TestResolveVerifiesReleaseMetadataWhenPresent(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("e", 40)
	tag := "v1.0.0"
	remote := &fakeGitHub{
		releases: map[string]Release{tag: {ID: 5, Tag: tag}},
		commits:  map[string]string{tag: commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "verified\n")},
	}
	resolver := NewResolver(remote)
	baseline, err := resolver.Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
	if err != nil {
		t.Fatal(err)
	}
	remote.releases[tag] = Release{ID: 5, Tag: tag, Assets: []ReleaseAsset{{ID: 9, Name: "acr-package.json"}}}
	remote.assets = map[int64][]byte{9: releaseMetadataJSON(1, commit, baseline.ContentHash)}
	verified, err := resolver.Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
	if err != nil {
		t.Fatal(err)
	}
	if verified.ContentHash != baseline.ContentHash || remote.assetCalls != 1 {
		t.Fatalf("Resolve() = %#v, asset calls = %d", verified, remote.assetCalls)
	}
}

func TestResolveFailsOnMetadataCommitMismatch(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("f", 40)
	tag := "v1.0.0"
	remote := &fakeGitHub{
		releases: map[string]Release{tag: {ID: 6, Tag: tag, Assets: []ReleaseAsset{{ID: 10, Name: "acr-package.json"}}}},
		commits:  map[string]string{tag: commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "content\n")},
		assets:   map[int64][]byte{10: releaseMetadataJSON(1, strings.Repeat("a", 40), "sha256:"+strings.Repeat("0", 64))},
	}
	_, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
	if err == nil || !strings.Contains(err.Error(), "metadata commit mismatch") || !strings.Contains(err.Error(), "do not use") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolveFailsOnMetadataContentHashMismatch(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("1", 40)
	tag := "v1.0.0"
	remote := &fakeGitHub{
		releases: map[string]Release{tag: {ID: 7, Tag: tag, Assets: []ReleaseAsset{{ID: 11, Name: "acr-package.json"}}}},
		commits:  map[string]string{tag: commit},
		archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "content\n")},
		assets:   map[int64][]byte{11: releaseMetadataJSON(1, commit, "sha256:"+strings.Repeat("0", 64))},
	}
	_, err := NewResolver(remote).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag})
	if err == nil || !strings.Contains(err.Error(), "metadata content hash mismatch") || !strings.Contains(err.Error(), "do not use") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolveProceedsWhenMetadataIsAbsentUnavailableOrUnknown(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("2", 40)
	tag := "v1.0.0"
	archive := packageArchive(t, "1.0.0", "content\n")
	tests := []struct {
		name        string
		release     Release
		assets      map[int64][]byte
		assetErr    error
		wantWarning string
	}{
		{name: "absent", release: Release{ID: 8, Tag: tag}},
		{name: "unavailable", release: Release{ID: 8, Tag: tag, Assets: []ReleaseAsset{{ID: 12, Name: "acr-package.json"}}}, assetErr: errors.New("temporary asset failure"), wantWarning: "download failed: temporary asset failure"},
		{name: "unknown version", release: Release{ID: 8, Tag: tag, Assets: []ReleaseAsset{{ID: 12, Name: "acr-package.json"}}}, assets: map[int64][]byte{12: releaseMetadataJSON(2, "", "")}, wantWarning: "unsupported metadataVersion 2"},
		{name: "malformed", release: Release{ID: 8, Tag: tag, Assets: []ReleaseAsset{{ID: 12, Name: "acr-package.json"}}}, assets: map[int64][]byte{12: []byte("not json")}, wantWarning: "malformed JSON"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			remote := &fakeGitHub{
				releases: map[string]Release{tag: test.release}, commits: map[string]string{tag: commit},
				archives: map[string][]byte{commit: archive}, assets: test.assets, assetErr: test.assetErr,
			}
			if _, err := newResolver(remote, &stderr).Resolve(context.Background(), Declaration{Source: "github:owner/plugin", Requested: tag}); err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if test.wantWarning == "" {
				if stderr.Len() != 0 {
					t.Fatalf("Resolve() stderr = %q, want empty", stderr.String())
				}
				return
			}
			if !strings.Contains(stderr.String(), tag) || !strings.Contains(stderr.String(), test.wantWarning) || bytes.Count(stderr.Bytes(), []byte("\n")) != 1 {
				t.Fatalf("Resolve() stderr = %q, want one warning naming tag %q and reason %q", stderr.String(), tag, test.wantWarning)
			}
		})
	}
}

func releaseMetadataJSON(version int, commit, contentHash string) []byte {
	return []byte(fmt.Sprintf(`{"metadataVersion":%d,"commit":%q,"contentHash":%q}`, version, commit, contentHash))
}

func TestTagMatchesVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		tag     string
		version string
		want    bool
	}{
		{tag: "v1.2.3", version: "1.2.3", want: true},
		{tag: "1.2.3", version: "1.2.3", want: true},
		{tag: "vv1.2.3", version: "1.2.3", want: false},
		{tag: "v1.2.4", version: "1.2.3", want: false},
	} {
		if got := TagMatchesVersion(test.tag, test.version); got != test.want {
			t.Errorf("TagMatchesVersion(%q, %q) = %t, want %t", test.tag, test.version, got, test.want)
		}
	}
}

func TestVerifyLockedRejectsHashMismatchWithoutResolution(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("d", 40)
	remote := &fakeGitHub{archives: map[string][]byte{commit: packageArchive(t, "1.0.0", "changed\n")}}
	locked := LockedDependency{Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, Tag: "v1.0.0", Commit: commit, PackageVersion: "1.0.0", ContentHash: "sha256:wrong"}

	err := NewResolver(remote).VerifyLocked(context.Background(), locked)
	if err == nil || !strings.Contains(err.Error(), "content hash mismatch") || !strings.Contains(err.Error(), "do not use") {
		t.Fatalf("VerifyLocked() error = %v, want actionable hash mismatch", err)
	}
	if remote.latestCalls != 0 || remote.releaseCalls != 0 || remote.resolveCalls != 0 || remote.downloadCalls != 1 {
		t.Fatalf("VerifyLocked() remote calls = %#v", remote)
	}
}

type fakeGitHub struct {
	latest        Release
	releases      map[string]Release
	commits       map[string]string
	archives      map[string][]byte
	err           error
	latestCalls   int
	releaseCalls  int
	resolveCalls  int
	downloadCalls int
	assetCalls    int
	assets        map[int64][]byte
	assetErr      error
}

func (fake *fakeGitHub) LatestRelease(_ context.Context, _ Repository) (Release, error) {
	fake.latestCalls++
	if fake.err != nil {
		return Release{}, fake.err
	}
	if fake.latest.Tag == "" {
		return Release{}, errors.New("no stable release; publish a stable GitHub Release and retry")
	}
	return fake.latest, nil
}

func (fake *fakeGitHub) ReleaseByTag(_ context.Context, _ Repository, tag string) (Release, error) {
	fake.releaseCalls++
	if fake.err != nil {
		return Release{}, fake.err
	}
	release, exists := fake.releases[tag]
	if !exists {
		return Release{}, errors.New("release not found; choose an existing stable tag")
	}
	return release, nil
}

func (fake *fakeGitHub) ResolveCommit(_ context.Context, _ Repository, reference string) (string, error) {
	fake.resolveCalls++
	if fake.err != nil {
		return "", fake.err
	}
	commit, exists := fake.commits[reference]
	if !exists {
		return "", errors.New("commit not found; verify the reference")
	}
	return commit, nil
}

func (fake *fakeGitHub) DownloadArchive(_ context.Context, _ Repository, commit string) ([]byte, error) {
	fake.downloadCalls++
	if fake.err != nil {
		return nil, fake.err
	}
	archive, exists := fake.archives[commit]
	if !exists {
		return nil, errors.New("archive not found; verify repository access")
	}
	return archive, nil
}

func (fake *fakeGitHub) DownloadReleaseAsset(_ context.Context, _ Repository, asset ReleaseAsset) ([]byte, error) {
	fake.assetCalls++
	if fake.assetErr != nil {
		return nil, fake.assetErr
	}
	contents, exists := fake.assets[asset.ID]
	if !exists {
		return nil, errors.New("asset not found")
	}
	return append([]byte(nil), contents...), nil
}

func packageArchive(t *testing.T, version, contents string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: owner/plugin\nversion: " + version + "\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"
	return testArchive(t, "owner-plugin-commit", map[string]string{"agent-plugin.yaml": manifest, "guidance.md": contents})
}
