package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceReconcileRefreshesLatestAndPreservesPins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	oldLatest := strings.Repeat("a", 40)
	newLatest := strings.Repeat("b", 40)
	pinned := strings.Repeat("c", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: "github:owner/plugin", Requested: "latest"},
			{Source: "github:owner/pinned", Requested: pinned[:12]},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0", Commit: oldLatest, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64)},
			{Source: "github:owner/pinned", Requested: pinned[:12], Kind: ResolutionCommit, Commit: pinned, PackageVersion: "3.0.0", ContentHash: "sha256:" + strings.Repeat("c", 64)},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{
		latest:   Release{ID: 2, Tag: "v2.0.0"},
		commits:  map[string]string{"v2.0.0": newLatest},
		archives: map[string][]byte{newLatest: packageArchive(t, "2.0.0", "new\n")},
	}
	service := NewService(NewResolver(remote))

	result, err := service.Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Changed || remote.latestCalls != 1 || remote.resolveCalls != 1 || remote.downloadCalls != 1 {
		t.Fatalf("Reconcile() = %#v, remote = %#v", result, remote)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if index, _ := findLock(loaded.Lock.Dependencies, "github:owner/plugin"); loaded.Lock.Dependencies[index].Commit != newLatest {
		t.Fatalf("latest lock = %#v", loaded.Lock.Dependencies[index])
	}
	if index, _ := findLock(loaded.Lock.Dependencies, "github:owner/pinned"); loaded.Lock.Dependencies[index].Commit != pinned {
		t.Fatalf("pinned lock changed = %#v", loaded.Lock.Dependencies[index])
	}
}

func TestServiceDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("d", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 4, Tag: "v4.0.0"},
		commits:  map[string]string{"v4.0.0": commit},
		archives: map[string][]byte{commit: packageArchive(t, "4.0.0", "dry\n")},
	}
	result, err := NewService(NewResolver(remote)).Install(context.Background(), root, "github:owner/plugin", "latest", DowngradeUnset, true)
	if err != nil {
		t.Fatalf("Install(dry-run) error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Install(dry-run) Changed = false, want true")
	}
	for _, relative := range []string{ProjectFilename, LockFilename} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("dry-run created %s: %v", relative, err)
		}
	}
}

func TestServiceInstallSamePinDoesNotResolveAgain(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	requested := strings.Repeat("a", 12)
	commit := strings.Repeat("a", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: requested}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: requested, Kind: ResolutionCommit, Commit: commit,
			PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("b", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{err: errors.New("remote must not be called")}

	result, err := NewService(NewResolver(remote)).Install(context.Background(), root, "github:owner/plugin", requested, DowngradeUnset, false)
	if err != nil {
		t.Fatalf("Install(same pin) error = %v", err)
	}
	if result.Changed || remote.releaseCalls != 0 || remote.resolveCalls != 0 || remote.downloadCalls != 0 {
		t.Fatalf("Install(same pin) = %#v, remote = %#v", result, remote)
	}
}

func TestServiceOutdatedIsReadOnlyAndSkipsPins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	current := strings.Repeat("e", 40)
	latest := strings.Repeat("f", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: "github:owner/plugin", Requested: "latest"},
			{Source: "github:owner/pinned", Requested: current[:12]},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0", Commit: current, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("e", 64)},
			{Source: "github:owner/pinned", Requested: current[:12], Kind: ResolutionCommit, Commit: current, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("e", 64)},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	projectBefore := readTestFile(t, filepath.Join(root, ProjectFilename))
	lockBefore := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename)))
	remote := &fakeGitHub{latest: Release{ID: 2, Tag: "v2.0.0"}, commits: map[string]string{"v2.0.0": latest}}

	outdated, err := NewService(NewResolver(remote)).Outdated(context.Background(), root)
	if err != nil {
		t.Fatalf("Outdated() error = %v", err)
	}
	if len(outdated) != 1 || outdated[0].Source != "github:owner/plugin" || outdated[0].LatestCommit != latest {
		t.Fatalf("Outdated() = %#v", outdated)
	}
	if remote.downloadCalls != 0 || remote.latestCalls != 1 || remote.resolveCalls != 1 {
		t.Fatalf("Outdated() remote calls = %#v", remote)
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != projectBefore {
		t.Fatal("Outdated() modified agents.yaml")
	}
	if got := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename))); got != lockBefore {
		t.Fatal("Outdated() modified lockfile")
	}
}

func TestServiceOutdatedDetectsNewReleaseAtSameCommit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: "latest"}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: commit, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{latest: Release{ID: 2, Tag: "v1.0.1"}, commits: map[string]string{"v1.0.1": commit}}

	outdated, err := NewService(NewResolver(remote)).Outdated(context.Background(), root)
	if err != nil {
		t.Fatalf("Outdated() error = %v", err)
	}
	if len(outdated) != 1 || outdated[0].CurrentTag != "v1.0.0" || outdated[0].LatestTag != "v1.0.1" || outdated[0].CurrentCommit != outdated[0].LatestCommit {
		t.Fatalf("Outdated() = %#v, want new release identity at same commit", outdated)
	}
}

func TestServiceUpdateRejectsUndeclaredSource(t *testing.T) {
	t.Parallel()

	_, err := NewService(NewResolver(&fakeGitHub{})).Update(context.Background(), t.TempDir(), "github:owner/missing", false)
	var notDeclared *NotDeclaredError
	if !errors.As(err, &notDeclared) || !strings.Contains(err.Error(), "acr list") {
		t.Fatalf("Update() error = %v, want a typed refusal naming acr list", err)
	}
}

func TestServiceUpdateRefreshesLatestAndPreservesPins(t *testing.T) {
	t.Parallel()

	latestSource := "github:owner/plugin"
	pinnedSource := "github:owner/pinned"
	oldLatest := strings.Repeat("a", 40)
	newLatest := strings.Repeat("b", 40)
	pinned := strings.Repeat("c", 40)
	tests := []struct {
		name          string
		source        string
		dryRun        bool
		wantResolve   bool
		wantPersisted bool
	}{
		{name: "all latest", wantResolve: true, wantPersisted: true},
		{name: "specific latest dry run", source: latestSource, dryRun: true, wantResolve: true},
		{name: "specific pin", source: pinnedSource},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state := State{
				Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
					{Source: latestSource, Requested: "latest"},
					{Source: pinnedSource, Requested: pinned[:12]},
				}},
				Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
					{Source: latestSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0", Commit: oldLatest, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64)},
					{Source: pinnedSource, Requested: pinned[:12], Kind: ResolutionCommit, Commit: pinned, PackageVersion: "3.0.0", ContentHash: "sha256:" + strings.Repeat("c", 64)},
				}},
			}
			if err := WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			remote := &fakeGitHub{
				latest:   Release{ID: 2, Tag: "v2.0.0"},
				commits:  map[string]string{"v2.0.0": newLatest},
				archives: map[string][]byte{newLatest: packageArchive(t, "2.0.0", "new\n")},
			}

			result, err := NewService(NewResolver(remote)).Update(context.Background(), root, test.source, test.dryRun)
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			wantCalls := 0
			if test.wantResolve {
				wantCalls = 1
			}
			if remote.latestCalls != wantCalls || remote.resolveCalls != wantCalls || remote.downloadCalls != wantCalls {
				t.Fatalf("Update() remote calls = %#v, want %d resolution", remote, wantCalls)
			}
			if result.Changed != test.wantResolve {
				t.Fatalf("Update() Changed = %t, want %t", result.Changed, test.wantResolve)
			}
			resultLatest, _ := findLock(result.Dependencies, latestSource)
			wantResultLatest := oldLatest
			if test.wantResolve {
				wantResultLatest = newLatest
			}
			if result.Dependencies[resultLatest].Commit != wantResultLatest {
				t.Fatalf("Update() latest result = %#v, want commit %s", result.Dependencies[resultLatest], wantResultLatest)
			}
			resultPin, _ := findLock(result.Dependencies, pinnedSource)
			if result.Dependencies[resultPin].Commit != pinned {
				t.Fatalf("Update() changed pinned result = %#v", result.Dependencies[resultPin])
			}

			loaded, err := LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			persistedLatest, _ := findLock(loaded.Lock.Dependencies, latestSource)
			wantPersistedLatest := oldLatest
			if test.wantPersisted {
				wantPersistedLatest = newLatest
			}
			if loaded.Lock.Dependencies[persistedLatest].Commit != wantPersistedLatest {
				t.Fatalf("persisted latest lock = %#v, want commit %s", loaded.Lock.Dependencies[persistedLatest], wantPersistedLatest)
			}
			persistedPin, _ := findLock(loaded.Lock.Dependencies, pinnedSource)
			if loaded.Lock.Dependencies[persistedPin].Commit != pinned {
				t.Fatalf("persisted pinned lock changed = %#v", loaded.Lock.Dependencies[persistedPin])
			}
		})
	}
}
