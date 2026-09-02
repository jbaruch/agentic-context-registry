package dependency

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolverLocksLatestReleaseAndVerifiesContent(t *testing.T) {
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

func TestResolverKeepsCommitPinMetadataImmutable(t *testing.T) {
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

func packageArchive(t *testing.T, version, contents string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: owner/plugin\nversion: " + version + "\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"
	return testArchive(t, "owner-plugin-commit", map[string]string{"agent-plugin.yaml": manifest, "guidance.md": contents})
}
