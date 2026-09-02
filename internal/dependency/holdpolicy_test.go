package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	heldTag      = "v1.3.2"
	rejectedTag  = "v1.4.0"
	heldSource   = "github:owner/plugin"
	siblingSourc = "github:owner/sibling"
)

// heldProject writes a project holding heldSource at heldTag after rejecting
// rejectedTag, locked to the supplied commit.
func heldProject(t *testing.T, commit string) string {
	t.Helper()
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag, Reason: "1.4.0 breaks the review hook"},
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987,
			Tag: heldTag, Commit: commit, PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
			Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024, RejectedCommit: strings.Repeat("c", 40)},
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

func readStateFiles(t *testing.T, root string) (string, string) {
	t.Helper()
	return readTestFile(t, filepath.Join(root, ProjectFilename)), readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename)))
}

func TestReconcilePreservesHeldRelease(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	projectBefore, lockBefore := readStateFiles(t, root)
	rejectedCommit := strings.Repeat("d", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		commits:  map[string]string{rejectedTag: rejectedCommit},
		archives: map[string][]byte{rejectedCommit: packageArchive(t, "1.4.0", "rejected\n")},
	}

	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Changed || len(result.Notices) != 0 {
		t.Fatalf("Reconcile() = %#v, want a silent steady state", result)
	}
	if remote.downloadCalls != 0 {
		t.Fatalf("Reconcile() downloaded the rejected release: %#v", remote)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatalf("Reconcile() rewrote held state:\n%s\n%s", projectAfter, lockAfter)
	}
}

func TestUpdateNeverClearsHold(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	newerCommit := strings.Repeat("e", 40)
	for _, source := range []string{"", heldSource} {
		t.Run("source="+source, func(t *testing.T) {
			root := heldProject(t, commit)
			projectBefore, lockBefore := readStateFiles(t, root)
			remote := &fakeGitHub{
				latest:   Release{ID: 2048, Tag: "v1.4.1"},
				commits:  map[string]string{"v1.4.1": newerCommit},
				archives: map[string][]byte{newerCommit: packageArchive(t, "1.4.1", "newer\n")},
			}

			result, err := NewService(NewResolver(remote)).Update(context.Background(), root, source, false)
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if result.Changed || remote.downloadCalls != 0 {
				t.Fatalf("Update() = %#v, remote = %#v, want the hold preserved", result, remote)
			}
			if len(result.Notices) != 1 || !strings.Contains(result.Notices[0], "acr resume "+heldSource) {
				t.Fatalf("Update() notices = %#v, want one resume suggestion", result.Notices)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatalf("Update() rewrote held state:\n%s\n%s", projectAfter, lockAfter)
			}
		})
	}
}

func TestHoldBarrierStands(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	tests := map[string]Release{
		"barrier republished as a new release": {ID: 4096, Tag: rejectedTag},
		"barrier retagged onto a new commit":   {ID: 1024, Tag: rejectedTag},
		"draft beyond the barrier":             {ID: 4096, Tag: "v1.4.1", Draft: true},
		"prerelease beyond the barrier":        {ID: 4096, Tag: "v1.4.1-rc.1", Prerelease: true},
		"older stable release":                 {ID: 512, Tag: "v1.3.9"},
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			root := heldProject(t, commit)
			projectBefore, lockBefore := readStateFiles(t, root)
			candidateCommit := strings.Repeat("f", 40)
			remote := &fakeGitHub{
				latest:   candidate,
				commits:  map[string]string{candidate.Tag: candidateCommit},
				archives: map[string][]byte{candidateCommit: packageArchive(t, strings.TrimPrefix(candidate.Tag, "v"), "candidate\n")},
			}

			result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.Changed || len(result.Notices) != 0 || remote.downloadCalls != 0 {
				t.Fatalf("Reconcile() = %#v, remote = %#v, want the barrier to stand silently", result, remote)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatalf("Reconcile() rewrote held state for %s", name)
			}
		})
	}
}

func TestHoldSuggestsResumeBeyondTheBarrier(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	_, lockBefore := readStateFiles(t, root)
	newerCommit := strings.Repeat("e", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 2048, Tag: "v1.4.1"},
		commits:  map[string]string{"v1.4.1": newerCommit},
		archives: map[string][]byte{newerCommit: packageArchive(t, "1.4.1", "newer\n")},
	}

	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(result.Notices) != 1 {
		t.Fatalf("Reconcile() notices = %#v, want one resume suggestion", result.Notices)
	}
	notice := result.Notices[0]
	for _, want := range []string{heldSource, heldTag, rejectedTag, "v1.4.1", "acr resume " + heldSource} {
		if !strings.Contains(notice, want) {
			t.Fatalf("resume suggestion %q does not name %q", notice, want)
		}
	}
	if result.Changed || remote.downloadCalls != 0 {
		t.Fatalf("Reconcile() adopted the candidate beyond the barrier: %#v", result)
	}
	if _, lockAfter := readStateFiles(t, root); lockAfter != lockBefore {
		t.Fatal("Reconcile() rewrote the lock for a candidate beyond the barrier")
	}
}

func TestStrictlyNewerFallsBackToReleaseID(t *testing.T) {
	t.Parallel()

	barrier := &LockedDependency{Hold: &LockHold{RejectedTag: "release-20260901", RejectedReleaseID: 1024}}
	hold := &Hold{Pin: "release-20260801", Rejected: "release-20260901"}
	tests := []struct {
		name      string
		candidate Release
		existing  *LockedDependency
		want      bool
	}{
		{name: "newer release id", candidate: Release{ID: 2048, Tag: "release-20261001"}, existing: barrier, want: true},
		{name: "older release id", candidate: Release{ID: 512, Tag: "release-20260801"}, existing: barrier},
		{name: "equal release id", candidate: Release{ID: 1024, Tag: "release-20260901-retag"}, existing: barrier},
		{name: "no recorded barrier id", candidate: Release{ID: 2048, Tag: "release-20261001"}, existing: &LockedDependency{}},
		{name: "no existing lock", candidate: Release{ID: 2048, Tag: "release-20261001"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := beyondBarrier(test.candidate, test.existing, hold); got != test.want {
				t.Fatalf("beyondBarrier(%q) = %t, want %t", test.candidate.Tag, got, test.want)
			}
		})
	}
}

func TestSemverBarrierComparison(t *testing.T) {
	t.Parallel()

	hold := &Hold{Pin: heldTag, Rejected: rejectedTag}
	tests := []struct {
		tag  string
		want bool
	}{
		{tag: "v1.4.1", want: true},
		{tag: "v1.5.0", want: true},
		{tag: "v2.0.0", want: true},
		{tag: "1.4.1", want: true},
		{tag: rejectedTag},
		{tag: "v1.4.0+build.7"},
		{tag: "v1.3.9"},
		{tag: "v1.4.1-rc.1"},
		{tag: "v0.9.0"},
	}
	for _, test := range tests {
		t.Run(test.tag, func(t *testing.T) {
			got := beyondBarrier(Release{ID: 4096, Tag: test.tag}, nil, hold)
			if got != test.want {
				t.Fatalf("beyondBarrier(%q) = %t, want %t", test.tag, got, test.want)
			}
		})
	}
}

func TestHoldDoesNotBlockSiblingLatestRefresh(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	heldCommit := strings.Repeat("a", 40)
	siblingOld := strings.Repeat("1", 40)
	siblingNew := strings.Repeat("2", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}},
			{Source: siblingSourc, Requested: "latest"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
				Commit: heldCommit, PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
				Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024}},
			{Source: siblingSourc, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 5, Tag: "v2.0.0",
				Commit: siblingOld, PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &perSourceGitHub{
		latest:   map[string]Release{heldSource: {ID: 1024, Tag: rejectedTag}, siblingSourc: {ID: 6, Tag: "v2.1.0"}},
		commits:  map[string]string{heldSource + "@" + rejectedTag: strings.Repeat("d", 40), siblingSourc + "@v2.1.0": siblingNew},
		archives: map[string][]byte{siblingNew: packageArchiveFor(t, "owner/sibling", "2.1.0", "sibling\n")},
	}

	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Reconcile() Changed = false, want the sibling refreshed")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	heldIndex, _ := findLock(loaded.Lock.Dependencies, heldSource)
	if loaded.Lock.Dependencies[heldIndex].Commit != heldCommit || loaded.Lock.Dependencies[heldIndex].Tag != heldTag {
		t.Fatalf("held lock changed: %#v", loaded.Lock.Dependencies[heldIndex])
	}
	siblingIndex, _ := findLock(loaded.Lock.Dependencies, siblingSourc)
	if loaded.Lock.Dependencies[siblingIndex].Commit != siblingNew {
		t.Fatalf("sibling lock did not advance: %#v", loaded.Lock.Dependencies[siblingIndex])
	}
	if loaded.Lock.Dependencies[siblingIndex].Hold != nil {
		t.Fatalf("the hold leaked onto the sibling: %#v", loaded.Lock.Dependencies[siblingIndex].Hold)
	}
	siblingDeclaration, _ := findDeclaration(loaded.Project.Dependencies, siblingSourc)
	if loaded.Project.Dependencies[siblingDeclaration].Hold != nil {
		t.Fatalf("the hold leaked onto the sibling declaration: %#v", loaded.Project.Dependencies[siblingDeclaration].Hold)
	}
	if remote.downloadCalls != 1 {
		t.Fatalf("Reconcile() downloads = %d, want only the sibling", remote.downloadCalls)
	}
}

func TestHoldRestoresADeletedLockWithoutCrossingTheBarrier(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(LockFilename))); err != nil {
		t.Fatal(err)
	}
	heldCommit := strings.Repeat("7", 40)
	rejectedCommit := strings.Repeat("8", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		releases: map[string]Release{heldTag: {ID: 987, Tag: heldTag}, rejectedTag: {ID: 1024, Tag: rejectedTag}},
		commits:  map[string]string{heldTag: heldCommit, rejectedTag: rejectedCommit},
		archives: map[string][]byte{heldCommit: packageArchive(t, "1.3.2", "known good\n")},
	}

	if _, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Tag != heldTag || locked.Commit != heldCommit || locked.Requested != "latest" {
		t.Fatalf("restored lock = %#v, want the held release under a latest request", locked)
	}
	if locked.Hold == nil || locked.Hold.RejectedTag != rejectedTag || locked.Hold.RejectedReleaseID != 1024 || locked.Hold.RejectedCommit != rejectedCommit {
		t.Fatalf("restored barrier record = %#v", locked.Hold)
	}
}

func TestHoldToleratesAMissingBarrierRelease(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(LockFilename))); err != nil {
		t.Fatal(err)
	}
	heldCommit := strings.Repeat("7", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 2048, Tag: "v1.4.1"},
		releases: map[string]Release{heldTag: {ID: 987, Tag: heldTag}},
		commits:  map[string]string{heldTag: heldCommit, "v1.4.1": strings.Repeat("9", 40)},
		archives: map[string][]byte{heldCommit: packageArchive(t, "1.3.2", "known good\n")},
	}

	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Tag != heldTag || locked.Hold == nil || locked.Hold.RejectedTag != rejectedTag {
		t.Fatalf("restored lock = %#v, want tag-only barrier matching", locked)
	}
	if locked.Hold.RejectedReleaseID != 0 || locked.Hold.RejectedCommit != "" {
		t.Fatalf("barrier record invented an identity for a deleted release: %#v", locked.Hold)
	}
	if len(result.Notices) != 1 || !strings.Contains(result.Notices[0], "acr resume") {
		t.Fatalf("Reconcile() notices = %#v, want the resume suggestion", result.Notices)
	}
}

// A dependency re-declared as a held latest carries a lock still stamped with
// its previous fixed request. The hold must repair that in place rather than
// preserve a stale policy the lock validator would reject (#35).
func TestHoldRepairsAStaleRequestedPolicyOffline(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	declaration := Declaration{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}}
	existing := &LockedDependency{
		Source: heldSource, Requested: heldTag, Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
		Commit: commit, PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
	}
	remote := &fakeGitHub{err: errors.New("remote must not be called")}

	decision, err := NewHoldPolicy(NewResolver(remote)).Resolve(context.Background(), declaration, existing, Release{ID: 1024, Tag: rejectedTag})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decision.Skip || decision.Pin == nil {
		t.Fatalf("Resolve() = %#v, want a repaired pin", decision)
	}
	if decision.Pin.Requested != "latest" || decision.Pin.Commit != commit || decision.Pin.Tag != heldTag {
		t.Fatalf("repaired pin = %#v", decision.Pin)
	}
	if decision.Pin.Hold == nil || decision.Pin.Hold.RejectedTag != rejectedTag {
		t.Fatalf("repaired barrier = %#v", decision.Pin.Hold)
	}
	if err := validateHeldPin(declaration, *decision.Pin); err != nil {
		t.Fatalf("repaired pin fails lock validation: %v", err)
	}
	if remote.latestCalls != 0 || remote.releaseCalls != 0 || remote.resolveCalls != 0 || remote.downloadCalls != 0 {
		t.Fatalf("offline repair contacted the remote: %#v", remote)
	}
}

func TestHoldSkipsWhenTheLockAlreadyMatchesTheDeclaration(t *testing.T) {
	t.Parallel()

	declaration := Declaration{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}}
	existing := &LockedDependency{
		Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
		Commit: strings.Repeat("a", 40), PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
		Hold: &LockHold{RejectedTag: rejectedTag},
	}
	remote := &fakeGitHub{err: errors.New("remote must not be called")}

	decision, err := NewHoldPolicy(NewResolver(remote)).Resolve(context.Background(), declaration, existing, Release{ID: 1024, Tag: rejectedTag})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !decision.Skip || decision.Pin != nil || decision.Notice != "" {
		t.Fatalf("Resolve() = %#v, want a silent skip", decision)
	}
}

func TestUnheldDeclarationIsANoOpForTheHoldPolicy(t *testing.T) {
	t.Parallel()

	remote := &fakeGitHub{err: errors.New("remote must not be called")}
	declaration := Declaration{Source: heldSource, Requested: "latest"}

	decision, err := NewHoldPolicy(NewResolver(remote)).Resolve(context.Background(), declaration, nil, Release{ID: 2, Tag: "v2.0.0"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decision.Skip || decision.Pin != nil || decision.Notice != "" {
		t.Fatalf("Resolve() = %#v, want a no-op for an unheld declaration", decision)
	}
}
