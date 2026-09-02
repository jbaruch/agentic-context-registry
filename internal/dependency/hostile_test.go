package dependency

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

type recordingHoldPolicy struct {
	sources []string
	pin     *LockedDependency
}

func (policy *recordingHoldPolicy) Resolve(_ context.Context, declaration Declaration, _ *LockedDependency, _ Release) (HoldDecision, error) {
	policy.sources = append(policy.sources, declaration.Source)
	if policy.pin != nil && declaration.Source == policy.pin.Source {
		return HoldDecision{Pin: policy.pin, Notice: "Held known-good release."}, nil
	}
	return HoldDecision{}, nil
}

type keyedGitHub struct {
	latest        map[string]Release
	commits       map[string]map[string]string
	archives      map[string][]byte
	latestCalls   []string
	releaseCalls  []string
	resolveCalls  []string
	downloadCalls []string
}

func (github *keyedGitHub) LatestRelease(_ context.Context, repository Repository) (Release, error) {
	github.latestCalls = append(github.latestCalls, repository.String())
	release, exists := github.latest[repository.String()]
	if !exists {
		return Release{}, fmt.Errorf("no latest release for %s", repository.String())
	}
	return release, nil
}

func (github *keyedGitHub) ReleaseByTag(_ context.Context, repository Repository, tag string) (Release, error) {
	github.releaseCalls = append(github.releaseCalls, repository.String()+"@"+tag)
	return Release{}, fmt.Errorf("unexpected ReleaseByTag for %s@%s", repository.String(), tag)
}

func (github *keyedGitHub) ResolveCommit(_ context.Context, repository Repository, reference string) (string, error) {
	github.resolveCalls = append(github.resolveCalls, repository.String()+"@"+reference)
	refs := github.commits[repository.String()]
	commit, exists := refs[reference]
	if !exists {
		return "", fmt.Errorf("unexpected ResolveCommit for %s@%s", repository.String(), reference)
	}
	return commit, nil
}

func (github *keyedGitHub) DownloadArchive(_ context.Context, repository Repository, commit string) ([]byte, error) {
	github.downloadCalls = append(github.downloadCalls, repository.String()+"@"+commit)
	archive, exists := github.archives[commit]
	if !exists {
		return nil, fmt.Errorf("unexpected DownloadArchive for %s@%s", repository.String(), commit)
	}
	return archive, nil
}

func TestHostileHoldPolicyConsultedForUnlockedLatestOutsideRefreshSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	heldPinCommit := strings.Repeat("a", 40)
	heldLatestCommit := strings.Repeat("b", 40)
	otherCommit := strings.Repeat("c", 40)
	otherNewCommit := strings.Repeat("d", 40)
	heldSource := "github:owner/held"
	otherSource := "github:owner/other"
	heldPin := LockedDependency{
		Source: heldSource, Requested: "latest", Kind: ResolutionRelease,
		ReleaseID: 1, Tag: "v1.0.0", Commit: heldPinCommit, PackageVersion: "1.0.0",
		ContentHash: "sha256:" + strings.Repeat("a", 64),
	}
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: heldSource, Requested: "latest"},
			{Source: otherSource, Requested: "latest"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: otherSource, Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: otherCommit, PackageVersion: "1.0.0",
			ContentHash: "sha256:" + strings.Repeat("c", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &keyedGitHub{
		latest: map[string]Release{
			heldSource:  {ID: 2, Tag: "v2.0.0"},
			otherSource: {ID: 2, Tag: "v2.0.0"},
		},
		commits: map[string]map[string]string{
			heldSource:  {"v2.0.0": heldLatestCommit},
			otherSource: {"v2.0.0": otherNewCommit},
		},
		archives: map[string][]byte{
			heldLatestCommit: repoArchive(t, "owner", "held", "2.0.0", "held-latest\n"),
			otherNewCommit:   repoArchive(t, "owner", "other", "2.0.0", "other-latest\n"),
		},
	}
	holds := &recordingHoldPolicy{pin: &heldPin}
	if _, err := NewServiceWithHoldPolicy(NewResolver(remote), holds).Install(context.Background(), root, otherSource, "latest", false); err != nil {
		t.Fatal(err)
	}
	if !containsString(holds.sources, heldSource) {
		t.Fatalf("HoldPolicy sources = %#v, want %s consulted while installing a different latest", holds.sources, heldSource)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	index, exists := findLock(loaded.Lock.Dependencies, heldSource)
	if !exists {
		t.Fatal("held declaration has no lock after install of another source")
	}
	if loaded.Lock.Dependencies[index].Commit != heldPinCommit {
		t.Fatalf("unlocked latest bypassed HoldPolicy: lock = %#v, want pin commit %s", loaded.Lock.Dependencies[index], heldPinCommit)
	}
	if containsString(remote.downloadCalls, heldSource+"@"+heldLatestCommit) {
		t.Fatalf("held latest archive was downloaded: %#v", remote.downloadCalls)
	}
}

func TestHostileHoldPolicyConsultedForUnlockedLatestDuringDryRunInstallOfDifferentSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	heldPinCommit := strings.Repeat("a", 40)
	heldLatestCommit := strings.Repeat("b", 40)
	otherCommit := strings.Repeat("c", 40)
	otherNewCommit := strings.Repeat("d", 40)
	heldSource := "github:owner/held"
	otherSource := "github:owner/other"
	heldPin := LockedDependency{
		Source: heldSource, Requested: "latest", Kind: ResolutionRelease,
		ReleaseID: 1, Tag: "v1.0.0", Commit: heldPinCommit, PackageVersion: "1.0.0",
		ContentHash: "sha256:" + strings.Repeat("a", 64),
	}
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: heldSource, Requested: "latest"},
			{Source: otherSource, Requested: "latest"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: otherSource, Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: otherCommit, PackageVersion: "1.0.0",
			ContentHash: "sha256:" + strings.Repeat("c", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	beforeAgents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ProjectFilename)))
	if err != nil {
		t.Fatal(err)
	}
	beforeLock, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	remote := &keyedGitHub{
		latest: map[string]Release{
			heldSource:  {ID: 2, Tag: "v2.0.0"},
			otherSource: {ID: 2, Tag: "v2.0.0"},
		},
		commits: map[string]map[string]string{
			heldSource:  {"v2.0.0": heldLatestCommit},
			otherSource: {"v2.0.0": otherNewCommit},
		},
		archives: map[string][]byte{
			heldLatestCommit: repoArchive(t, "owner", "held", "2.0.0", "held-latest\n"),
			otherNewCommit:   repoArchive(t, "owner", "other", "2.0.0", "other-latest\n"),
		},
	}
	holds := &recordingHoldPolicy{pin: &heldPin}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, &Application{service: NewServiceWithHoldPolicy(NewResolver(remote), holds), fallback: cli.UnavailableApplication{}}, "test").
		Run(context.Background(), []string{"install", otherSource, "--project", root, "--dry-run"})
	if exitCode != cli.ExitSuccess {
		t.Fatalf("dry-run install exit = %d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !containsString(holds.sources, heldSource) {
		t.Fatalf("HoldPolicy sources = %#v, want %s consulted during dry-run install of a different latest", holds.sources, heldSource)
	}
	afterAgents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ProjectFilename)))
	if err != nil {
		t.Fatal(err)
	}
	afterLock, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeAgents, afterAgents) || !bytes.Equal(beforeLock, afterLock) {
		t.Fatal("dry-run install wrote agents.yaml or the lockfile")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := findLock(loaded.Lock.Dependencies, heldSource); exists {
		t.Fatalf("dry-run install wrote a lock for %s: %#v", heldSource, loaded.Lock.Dependencies)
	}
	if containsString(remote.downloadCalls, heldSource+"@"+heldLatestCommit) {
		t.Fatalf("held latest archive was downloaded: %#v", remote.downloadCalls)
	}
}

func TestHostileReconcileLeavesTagAndSHAPinsUntouched(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	oldLatest := strings.Repeat("a", 40)
	newLatest := strings.Repeat("b", 40)
	tagPin := strings.Repeat("c", 40)
	shaPin := strings.Repeat("d", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: "github:owner/plugin", Requested: "latest"},
			{Source: "github:owner/tagged", Requested: "v1.2.3"},
			{Source: "github:owner/pinned", Requested: shaPin},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0", Commit: oldLatest, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64)},
			{Source: "github:owner/tagged", Requested: "v1.2.3", Kind: ResolutionRelease, ReleaseID: 9, Tag: "v1.2.3", Commit: tagPin, PackageVersion: "1.2.3", ContentHash: "sha256:" + strings.Repeat("c", 64)},
			{Source: "github:owner/pinned", Requested: shaPin, Kind: ResolutionCommit, Commit: shaPin, PackageVersion: "3.0.0", ContentHash: "sha256:" + strings.Repeat("d", 64)},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &keyedGitHub{
		latest: map[string]Release{"github:owner/plugin": {ID: 2, Tag: "v2.0.0"}},
		commits: map[string]map[string]string{
			"github:owner/plugin": {"v2.0.0": newLatest},
		},
		archives: map[string][]byte{newLatest: repoArchive(t, "owner", "plugin", "2.0.0", "new\n")},
	}
	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("Reconcile() Changed = false, want latest advanced")
	}
	if len(remote.releaseCalls) != 0 || containsPrefix(remote.resolveCalls, "github:owner/tagged") || containsPrefix(remote.resolveCalls, "github:owner/pinned") {
		t.Fatalf("pin lookup leaked: release=%#v resolve=%#v", remote.releaseCalls, remote.resolveCalls)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	assertLockCommit(t, loaded, "github:owner/plugin", newLatest)
	assertLockCommit(t, loaded, "github:owner/tagged", tagPin)
	assertLockCommit(t, loaded, "github:owner/pinned", shaPin)
}

func repoArchive(t *testing.T, owner, repo, version, contents string) []byte {
	t.Helper()
	manifest := fmt.Sprintf("schemaVersion: 1\nname: %s/%s\nversion: %s\nsource:\n  repository: https://github.com/%s/%s\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n", owner, repo, version, owner, repo)
	return testArchive(t, owner+"-"+repo+"-commit", map[string]string{"agent-plugin.yaml": manifest, "guidance.md": contents})
}

func assertLockCommit(t *testing.T, state State, source, commit string) {
	t.Helper()
	index, exists := findLock(state.Lock.Dependencies, source)
	if !exists {
		t.Fatalf("missing lock for %s", source)
	}
	if state.Lock.Dependencies[index].Commit != commit {
		t.Fatalf("%s commit = %s, want %s", source, state.Lock.Dependencies[index].Commit, commit)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
