package dependency

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// latestProject writes an unheld latest declaration locked at lockedTag.
func latestProject(t *testing.T, lockedTag, commit string) string {
	t.Helper()
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: heldSource, Requested: "latest"}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1024, Tag: lockedTag,
			Commit: commit, PackageVersion: strings.TrimPrefix(lockedTag, "v"), ContentHash: "sha256:" + strings.Repeat("9", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

// rollbackRemote answers for a repository whose latest release is rejectedTag
// and whose history also carries heldTag.
func rollbackRemote(t *testing.T, heldCommit, rejectedCommit string) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		releases: map[string]Release{heldTag: {ID: 987, Tag: heldTag}, rejectedTag: {ID: 1024, Tag: rejectedTag}},
		commits:  map[string]string{heldTag: heldCommit, rejectedTag: rejectedCommit},
		archives: map[string][]byte{heldCommit: packageArchive(t, "1.3.2", "known good\n")},
	}
}

// equalReferenceRemote answers for a repository whose only stable release is
// the one the lock already resolves, reachable by tag and by commit.
func equalReferenceRemote(t *testing.T, lockedCommit string) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		releases: map[string]Release{rejectedTag: {ID: 1024, Tag: rejectedTag}},
		commits:  map[string]string{rejectedTag: lockedCommit, lockedCommit: lockedCommit},
		archives: map[string][]byte{lockedCommit: packageArchive(t, "1.4.0", "current\n")},
	}
}

// An explicit reference equal to the release already locked moves the
// declaration nowhere, so it must never convert latest into a permanent pin on
// its own. Equality is a choice, exactly like a rollback.
func TestEqualReferenceRequiresADowngradeChoice(t *testing.T) {
	t.Parallel()

	lockedCommit := strings.Repeat("d", 40)
	for _, requested := range []string{rejectedTag, lockedCommit} {
		t.Run(requested, func(t *testing.T) {
			t.Parallel()
			root := latestProject(t, rejectedTag, lockedCommit)
			projectBefore, lockBefore := readStateFiles(t, root)
			remote := equalReferenceRemote(t, lockedCommit)

			_, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, requested, DowngradeUnset, false)

			var required *DowngradeRequiredError
			if !errors.As(err, &required) {
				t.Fatalf("Install(%s) error = %v, want DowngradeRequiredError", requested, err)
			}
			if required.CurrentTag != rejectedTag || required.RequestedRef != requested {
				t.Fatalf("DowngradeRequiredError = %#v", required)
			}
			if remote.downloadCalls != 0 {
				t.Fatalf("Install(%s) downloaded before a choice was made: %#v", requested, remote)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatalf("Install(%s) wrote state before a choice was made", requested)
			}
		})
	}
}

// --pin is the sanctioned way to stop tracking latest at the current release;
// --hold is refused, because a hold cannot pin the release it rejects.
func TestEqualReferenceHonoursTheChosenFlag(t *testing.T) {
	t.Parallel()

	lockedCommit := strings.Repeat("d", 40)
	for _, requested := range []string{rejectedTag, lockedCommit} {
		t.Run("pin/"+requested, func(t *testing.T) {
			t.Parallel()
			root := latestProject(t, rejectedTag, lockedCommit)
			remote := equalReferenceRemote(t, lockedCommit)

			if _, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, requested, DowngradePin, false); err != nil {
				t.Fatalf("Install(%s --pin) error = %v", requested, err)
			}
			loaded, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			declaration := loaded.Project.Dependencies[0]
			if declaration.Requested != requested || declaration.Hold != nil {
				t.Fatalf("--pin declaration = %#v, want a permanent pin at %s", declaration, requested)
			}
			if locked := loaded.Lock.Dependencies[0]; locked.Requested != requested || locked.Commit != lockedCommit || locked.Hold != nil {
				t.Fatalf("--pin lock = %#v", locked)
			}
		})

		t.Run("hold/"+requested, func(t *testing.T) {
			t.Parallel()
			root := latestProject(t, rejectedTag, lockedCommit)
			projectBefore, lockBefore := readStateFiles(t, root)
			remote := equalReferenceRemote(t, lockedCommit)

			_, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, requested, DowngradeHold, false)

			if err == nil || !strings.Contains(err.Error(), "the release the barrier would reject") || !strings.Contains(err.Error(), "--pin") {
				t.Fatalf("Install(%s --hold) error = %v, want a refusal naming --pin", requested, err)
			}
			if remote.downloadCalls != 0 {
				t.Fatalf("a refused --hold still downloaded: %#v", remote)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatal("a refused --hold still wrote state")
			}
		})
	}
}

func TestInstallDowngradeRequiresChoice(t *testing.T) {
	t.Parallel()

	rejectedCommit := strings.Repeat("d", 40)
	root := latestProject(t, rejectedTag, rejectedCommit)
	projectBefore, lockBefore := readStateFiles(t, root)
	remote := rollbackRemote(t, strings.Repeat("7", 40), rejectedCommit)

	_, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, heldTag, DowngradeUnset, false)
	var required *DowngradeRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("Install() error = %v, want DowngradeRequiredError", err)
	}
	if required.Source != heldSource || required.CurrentTag != rejectedTag || required.RequestedRef != heldTag {
		t.Fatalf("DowngradeRequiredError = %#v", required)
	}
	if !strings.Contains(err.Error(), "--hold") || !strings.Contains(err.Error(), "--pin") {
		t.Fatalf("Install() error %q does not name both choices", err)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatal("Install() wrote state before a choice was made")
	}
}

func TestInstallHoldFlagCreatesAReviewableRollback(t *testing.T) {
	t.Parallel()

	heldCommit := strings.Repeat("7", 40)
	rejectedCommit := strings.Repeat("d", 40)
	root := latestProject(t, rejectedTag, rejectedCommit)
	remote := rollbackRemote(t, heldCommit, rejectedCommit)

	result, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, heldTag, DowngradeHold, false)
	if err != nil {
		t.Fatalf("Install(--hold) error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Install(--hold) Changed = false")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	declaration := loaded.Project.Dependencies[0]
	if declaration.Requested != "latest" {
		t.Fatalf("--hold rewrote requested to %q", declaration.Requested)
	}
	if declaration.Hold == nil || declaration.Hold.Pin != heldTag || declaration.Hold.Rejected != rejectedTag {
		t.Fatalf("declared hold = %#v", declaration.Hold)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Tag != heldTag || locked.Commit != heldCommit || locked.PackageVersion != "1.3.2" {
		t.Fatalf("locked known-good = %#v", locked)
	}
	if locked.Hold == nil || locked.Hold.RejectedTag != rejectedTag || locked.Hold.RejectedReleaseID != 1024 || locked.Hold.RejectedCommit != rejectedCommit {
		t.Fatalf("locked barrier = %#v", locked.Hold)
	}
}

func TestInstallPinFlagRemovesHoldExplicitly(t *testing.T) {
	t.Parallel()

	heldCommit := strings.Repeat("7", 40)
	rejectedCommit := strings.Repeat("d", 40)
	root := latestProject(t, rejectedTag, rejectedCommit)
	remote := rollbackRemote(t, heldCommit, rejectedCommit)

	if _, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, heldTag, DowngradePin, false); err != nil {
		t.Fatalf("Install(--pin) error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	declaration := loaded.Project.Dependencies[0]
	if declaration.Requested != heldTag || declaration.Hold != nil {
		t.Fatalf("--pin declaration = %#v, want a permanent pin with no hold", declaration)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Requested != heldTag || locked.Tag != heldTag || locked.Hold != nil {
		t.Fatalf("--pin lock = %#v", locked)
	}
}

func TestPinFlagConvertsAnExistingHoldOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	heldCommit := strings.Repeat("7", 40)
	root := heldProject(t, heldCommit)
	remote := &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		releases: map[string]Release{"v1.3.0": {ID: 900, Tag: "v1.3.0"}},
		commits:  map[string]string{"v1.3.0": strings.Repeat("6", 40), rejectedTag: strings.Repeat("d", 40)},
		archives: map[string][]byte{strings.Repeat("6", 40): packageArchive(t, "1.3.0", "older\n")},
	}

	if _, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, "v1.3.0", DowngradePin, false); err != nil {
		t.Fatalf("Install(--pin) error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Requested != "v1.3.0" || loaded.Project.Dependencies[0].Hold != nil {
		t.Fatalf("declaration = %#v, want the hold replaced by a permanent pin", loaded.Project.Dependencies[0])
	}
	if loaded.Lock.Dependencies[0].Hold != nil {
		t.Fatalf("lock kept a barrier after --pin: %#v", loaded.Lock.Dependencies[0].Hold)
	}
}

func TestDeeperRollbackKeepsHighestBarrier(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("7", 40))
	olderCommit := strings.Repeat("6", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 1024, Tag: rejectedTag},
		releases: map[string]Release{"v1.3.0": {ID: 900, Tag: "v1.3.0"}, rejectedTag: {ID: 1024, Tag: rejectedTag}},
		commits:  map[string]string{"v1.3.0": olderCommit, rejectedTag: strings.Repeat("d", 40)},
		archives: map[string][]byte{olderCommit: packageArchive(t, "1.3.0", "older\n")},
	}

	if _, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, "v1.3.0", DowngradeHold, false); err != nil {
		t.Fatalf("Install(--hold) error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	hold := loaded.Project.Dependencies[0].Hold
	if hold == nil || hold.Pin != "v1.3.0" || hold.Rejected != rejectedTag {
		t.Fatalf("deeper rollback hold = %#v, want the higher barrier retained", hold)
	}
	if loaded.Lock.Dependencies[0].Commit != olderCommit {
		t.Fatalf("deeper rollback lock = %#v", loaded.Lock.Dependencies[0])
	}
}

func TestAdvanceBarrierOrdersRejectedReleases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *Hold
		rejected string
		want     string
	}{
		{name: "first rollback", rejected: rejectedTag, want: rejectedTag},
		{name: "deeper rollback keeps the higher barrier", existing: &Hold{Rejected: rejectedTag}, rejected: "v1.3.5", want: rejectedTag},
		{name: "second rollback advances", existing: &Hold{Rejected: rejectedTag}, rejected: "v1.4.1", want: "v1.4.1"},
		{name: "incomparable barriers take the new rejection", existing: &Hold{Rejected: "nightly"}, rejected: "v1.4.1", want: "v1.4.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := advanceBarrier(test.existing, test.rejected); got != test.want {
				t.Fatalf("advanceBarrier() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDowngradeFlagsRejectNonRollbacks(t *testing.T) {
	t.Parallel()

	newerCommit := strings.Repeat("e", 40)
	tests := []struct {
		name        string
		project     func(*testing.T) string
		requested   string
		choice      DowngradeChoice
		wantMessage string
	}{
		{
			name:        "hold on a newer release",
			project:     func(t *testing.T) string { return latestProject(t, heldTag, strings.Repeat("7", 40)) },
			requested:   "v1.4.1",
			choice:      DowngradeHold,
			wantMessage: "applies only to a rollback",
		},
		{
			name: "hold on a permanent pin",
			project: func(t *testing.T) string {
				root := t.TempDir()
				state := State{
					Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: heldSource, Requested: rejectedTag}}},
					Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
						Source: heldSource, Requested: rejectedTag, Kind: ResolutionRelease, ReleaseID: 1024, Tag: rejectedTag,
						Commit: strings.Repeat("d", 40), PackageVersion: "1.4.0", ContentHash: "sha256:" + strings.Repeat("9", 64),
					}}},
				}
				if err := WriteState(root, state); err != nil {
					t.Fatal(err)
				}
				return root
			},
			requested:   heldTag,
			choice:      DowngradeHold,
			wantMessage: "applies only to a rollback",
		},
		{
			name:        "pin on an undeclared source",
			project:     func(t *testing.T) string { return t.TempDir() },
			requested:   heldTag,
			choice:      DowngradePin,
			wantMessage: "has nothing to roll back",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := test.project(t)
			projectBefore, lockBefore := "", ""
			if test.name != "pin on an undeclared source" {
				projectBefore, lockBefore = readStateFiles(t, root)
			}
			remote := &fakeGitHub{
				latest:   Release{ID: 2048, Tag: "v1.4.1"},
				releases: map[string]Release{"v1.4.1": {ID: 2048, Tag: "v1.4.1"}, heldTag: {ID: 987, Tag: heldTag}},
				commits:  map[string]string{"v1.4.1": newerCommit, heldTag: strings.Repeat("7", 40)},
				archives: map[string][]byte{newerCommit: packageArchive(t, "1.4.1", "newer\n")},
			}

			_, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, test.requested, test.choice, false)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Install() error = %v, want %q", err, test.wantMessage)
			}
			if projectBefore != "" {
				projectAfter, lockAfter := readStateFiles(t, root)
				if projectAfter != projectBefore || lockAfter != lockBefore {
					t.Fatal("a rejected downgrade choice still wrote state")
				}
			}
		})
	}
}

// Re-declaring latest, or naming any explicit reference, must never quietly
// retire a barrier: acr resume and --pin are the only exits.
func TestInstallNeverSilentlyRetiresAHold(t *testing.T) {
	t.Parallel()

	t.Run("re-declaring latest preserves the hold", func(t *testing.T) {
		root := heldProject(t, strings.Repeat("a", 40))
		projectBefore, lockBefore := readStateFiles(t, root)
		remote := &fakeGitHub{
			latest:   Release{ID: 1024, Tag: rejectedTag},
			commits:  map[string]string{rejectedTag: strings.Repeat("d", 40)},
			archives: map[string][]byte{strings.Repeat("d", 40): packageArchive(t, "1.4.0", "rejected\n")},
		}

		result, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, "latest", DowngradeUnset, false)
		if err != nil {
			t.Fatalf("Install(latest) error = %v", err)
		}
		if result.Changed || remote.downloadCalls != 0 {
			t.Fatalf("Install(latest) = %#v, remote = %#v, want the hold preserved", result, remote)
		}
		projectAfter, lockAfter := readStateFiles(t, root)
		if projectAfter != projectBefore || lockAfter != lockBefore {
			t.Fatalf("Install(latest) retired the hold:\n%s\n%s", projectAfter, lockAfter)
		}
	})

	t.Run("a newer explicit tag still requires a choice", func(t *testing.T) {
		root := heldProject(t, strings.Repeat("a", 40))
		projectBefore, lockBefore := readStateFiles(t, root)
		remote := &fakeGitHub{
			latest:   Release{ID: 1024, Tag: rejectedTag},
			releases: map[string]Release{"v9.9.9": {ID: 4096, Tag: "v9.9.9"}},
			commits:  map[string]string{"v9.9.9": strings.Repeat("f", 40), rejectedTag: strings.Repeat("d", 40)},
			archives: map[string][]byte{strings.Repeat("f", 40): packageArchive(t, "9.9.9", "newest\n")},
		}

		_, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, "v9.9.9", DowngradeUnset, false)
		var required *DowngradeRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("Install(v9.9.9 over a hold) error = %v, want a required choice", err)
		}
		projectAfter, lockAfter := readStateFiles(t, root)
		if projectAfter != projectBefore || lockAfter != lockBefore {
			t.Fatal("a refused install still wrote state")
		}
	})
}

func TestInstallStillAcceptsOrdinaryUpgradesWithoutAChoice(t *testing.T) {
	t.Parallel()

	newerCommit := strings.Repeat("e", 40)
	root := latestProject(t, heldTag, strings.Repeat("7", 40))
	remote := &fakeGitHub{
		latest:   Release{ID: 2048, Tag: "v1.4.1"},
		releases: map[string]Release{"v1.4.1": {ID: 2048, Tag: "v1.4.1"}},
		commits:  map[string]string{"v1.4.1": newerCommit},
		archives: map[string][]byte{newerCommit: packageArchive(t, "1.4.1", "newer\n")},
	}

	result, err := NewService(NewResolver(remote)).Install(context.Background(), root, heldSource, "v1.4.1", DowngradeUnset, false)
	if err != nil {
		t.Fatalf("Install(newer tag) error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Install(newer tag) Changed = false")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Requested != "v1.4.1" || loaded.Project.Dependencies[0].Hold != nil {
		t.Fatalf("declaration = %#v, want a plain pin", loaded.Project.Dependencies[0])
	}
}
