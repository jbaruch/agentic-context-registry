package dependency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// Hostile verification for issue #17. Fixtures, names, and assertions are
// independent of the developer's hold_* tests: different sources, tags, and
// observable checks taken from the design note and the tester plan.

const (
	verifySource  = "github:acme/widget"
	verifySibling = "github:acme/gadget"
	verifyPin     = "v2.1.0"
	verifyBarrier = "v2.2.0"
	verifyNewer   = "v2.2.1"
)

type verifyRemote struct {
	latest        map[string]Release
	releases      map[string]Release
	commits       map[string]string
	archives      map[string][]byte
	assets        map[int64][]byte
	latestCalls   []string
	releaseCalls  []string
	resolveCalls  []string
	downloadCalls []string
	assetCalls    []string
}

func (remote *verifyRemote) LatestRelease(_ context.Context, repository Repository) (Release, error) {
	remote.latestCalls = append(remote.latestCalls, repository.String())
	release, exists := remote.latest[repository.String()]
	if !exists {
		return Release{}, fmt.Errorf("no latest release for %s", repository.String())
	}
	return release, nil
}

func (remote *verifyRemote) ReleaseByTag(_ context.Context, repository Repository, tag string) (Release, error) {
	key := repository.String() + "@" + tag
	remote.releaseCalls = append(remote.releaseCalls, key)
	release, exists := remote.releases[key]
	if !exists {
		return Release{}, fmt.Errorf("no release %s", key)
	}
	return release, nil
}

func (remote *verifyRemote) ResolveCommit(_ context.Context, repository Repository, reference string) (string, error) {
	key := repository.String() + "@" + reference
	remote.resolveCalls = append(remote.resolveCalls, key)
	commit, exists := remote.commits[key]
	if !exists {
		return "", fmt.Errorf("no commit %s", key)
	}
	return commit, nil
}

func (remote *verifyRemote) DownloadArchive(_ context.Context, repository Repository, commit string) ([]byte, error) {
	remote.downloadCalls = append(remote.downloadCalls, repository.String()+"@"+commit)
	archive, exists := remote.archives[commit]
	if !exists {
		return nil, fmt.Errorf("no archive %s", commit)
	}
	return archive, nil
}

func (remote *verifyRemote) DownloadReleaseAsset(_ context.Context, repository Repository, asset ReleaseAsset) ([]byte, error) {
	remote.assetCalls = append(remote.assetCalls, fmt.Sprintf("%s#%d", repository.String(), asset.ID))
	contents, exists := remote.assets[asset.ID]
	if !exists {
		return nil, fmt.Errorf("no asset %d", asset.ID)
	}
	return append([]byte(nil), contents...), nil
}

func verifyHeldState(pinCommit string) State {
	return State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: verifySource, Requested: "latest",
			Hold: &Hold{Pin: verifyPin, Rejected: verifyBarrier, Reason: "2.2.0 broke the review hook"},
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: verifySource, Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 21, Tag: verifyPin, Commit: pinCommit, PackageVersion: "2.1.0",
			ContentHash: "sha256:" + strings.Repeat("c", 64),
			Hold:        &LockHold{RejectedTag: verifyBarrier, RejectedReleaseID: 22, RejectedCommit: strings.Repeat("d", 40)},
		}}},
	}
}

func writeVerifyHeldProject(t *testing.T, pinCommit string) string {
	t.Helper()
	root := t.TempDir()
	if err := WriteState(root, verifyHeldState(pinCommit)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("operator notes must survive dry-run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func digestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(contents)
		digests[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return digests
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := digestTree(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("project tree changed:\nbefore %#v\nafter %#v", before, after)
	}
}

func TestVerifyHoldSilentSkipWhenCandidateEqualsBarrier(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	before := digestTree(t, root)
	projectBefore, lockBefore := readStateFiles(t, root)
	remote := &verifyRemote{
		latest: map[string]Release{verifySource: {ID: 22, Tag: verifyBarrier}},
		commits: map[string]string{
			verifySource + "@" + verifyBarrier: strings.Repeat("2", 40),
		},
		archives: map[string][]byte{strings.Repeat("2", 40): packageArchiveFor(t, "acme/widget", "2.2.0", "rejected\n")},
	}

	result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || len(result.Notices) != 0 {
		t.Fatalf("Reconcile() = %#v, want a silent skip", result)
	}
	if len(remote.downloadCalls) != 0 {
		t.Fatalf("silent skip downloaded archives: %#v", remote.downloadCalls)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatal("silent skip rewrote yaml or lock")
	}
	assertTreeUnchanged(t, root, before)

	outdated, err := NewService(NewResolver(remote)).Outdated(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range outdated {
		if row.Source == verifySource && row.Status == OutdatedUpdate {
			t.Fatalf("barrier-equal candidate classified as an ordinary update: %#v", row)
		}
	}
}

func TestVerifyNewerStableKeepsHoldAndSuggestsResumeOnStderrOnly(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	lockBefore := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename)))
	remote := &verifyRemote{
		latest:  map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		commits: map[string]string{verifySource + "@" + verifyNewer: strings.Repeat("3", 40)},
		archives: map[string][]byte{
			strings.Repeat("3", 40): packageArchiveFor(t, "acme/widget", "2.2.1", "newer\n"),
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), "test").Run(context.Background(), []string{"update", verifySource, "--project", root})
	if exitCode != cli.ExitSuccess {
		t.Fatalf("update exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), "acr resume") {
		t.Fatalf("resume suggestion leaked onto stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "dependency_hold_resumable") || !strings.Contains(stderr.String(), "acr resume "+verifySource) {
		t.Fatalf("stderr = %q, want a resume suggestion on stderr only", stderr.String())
	}
	if len(remote.downloadCalls) != 0 {
		t.Fatalf("newer-stable path downloaded the candidate: %#v", remote.downloadCalls)
	}
	if readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename))) != lockBefore {
		t.Fatal("newer stable rewrote the lock")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	hold := loaded.Project.Dependencies[0].Hold
	if hold == nil || hold.Pin != verifyPin || hold.Rejected != verifyBarrier {
		t.Fatalf("hold was mutated: %#v", hold)
	}
}

func TestVerifyHoldBarrierStandsOnUnstableAndRetaggedCandidates(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	candidates := map[string]Release{
		"draft":               {ID: 90, Tag: verifyNewer, Draft: true},
		"prerelease-flag":     {ID: 91, Tag: verifyNewer, Prerelease: true},
		"prerelease-tag":      {ID: 92, Tag: verifyNewer + "-rc.1"},
		"retag":               {ID: 2200, Tag: verifyBarrier},
		"same-tag-new-id":     {ID: 2201, Tag: verifyBarrier},
		"same-tag-new-commit": {ID: 22, Tag: verifyBarrier},
	}
	for name, candidate := range candidates {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := writeVerifyHeldProject(t, pinCommit)
			projectBefore, lockBefore := readStateFiles(t, root)
			candidateCommit := strings.Repeat("f", 40)
			remote := &verifyRemote{
				latest:   map[string]Release{verifySource: candidate},
				commits:  map[string]string{verifySource + "@" + candidate.Tag: candidateCommit},
				archives: map[string][]byte{candidateCommit: packageArchiveFor(t, "acme/widget", strings.TrimPrefix(candidate.Tag, "v"), "must-not-install\n")},
			}

			result, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Changed || len(result.Notices) != 0 || len(remote.downloadCalls) != 0 {
				t.Fatalf("%s: Reconcile() = %#v downloads=%v, want the barrier standing with zero archives", name, result, remote.downloadCalls)
			}
			projectAfter, lockAfter := readStateFiles(t, root)
			if projectAfter != projectBefore || lockAfter != lockBefore {
				t.Fatalf("%s rewrote state", name)
			}
		})
	}
}

func TestVerifyHeldSiblingLeavesUnheldLatestCallsAndDownloadsAlone(t *testing.T) {
	t.Parallel()

	heldCommit := strings.Repeat("1", 40)
	siblingOld := strings.Repeat("8", 40)
	siblingNew := strings.Repeat("9", 40)
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: verifySource, Requested: "latest", Hold: &Hold{Pin: verifyPin, Rejected: verifyBarrier}},
			{Source: verifySibling, Requested: "latest"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{
				Source: verifySource, Requested: "latest", Kind: ResolutionRelease,
				ReleaseID: 21, Tag: verifyPin, Commit: heldCommit, PackageVersion: "2.1.0",
				ContentHash: "sha256:" + strings.Repeat("c", 64),
				Hold:        &LockHold{RejectedTag: verifyBarrier, RejectedReleaseID: 22},
			},
			{
				Source: verifySibling, Requested: "latest", Kind: ResolutionRelease,
				ReleaseID: 80, Tag: "v3.0.0", Commit: siblingOld, PackageVersion: "3.0.0",
				ContentHash: "sha256:" + strings.Repeat("8", 64),
			},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	heldLockBefore := lockRow(t, root, verifySource)
	remote := &verifyRemote{
		latest: map[string]Release{
			verifySource:  {ID: 22, Tag: verifyBarrier},
			verifySibling: {ID: 81, Tag: "v3.1.0"},
		},
		releases: map[string]Release{
			verifySibling + "@v3.1.0": {ID: 81, Tag: "v3.1.0"},
		},
		commits: map[string]string{
			verifySibling + "@v3.1.0": siblingNew,
		},
		archives: map[string][]byte{
			siblingNew: packageArchiveFor(t, "acme/gadget", "3.1.0", "sibling\n"),
		},
	}

	if _, err := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
	if got := lockRow(t, root, verifySource); got != heldLockBefore {
		t.Fatalf("held lock moved:\n%s\n%s", heldLockBefore, got)
	}
	for _, call := range remote.latestCalls {
		if call != verifySource && call != verifySibling {
			t.Fatalf("unexpected latest call %q", call)
		}
	}
	if !containsString(remote.latestCalls, verifySibling) {
		t.Fatalf("sibling latest was not consulted: %#v", remote.latestCalls)
	}
	if len(remote.downloadCalls) != 1 || remote.downloadCalls[0] != verifySibling+"@"+siblingNew {
		t.Fatalf("downloads = %#v, want only the unheld sibling", remote.downloadCalls)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := findLock(loaded.Lock.Dependencies, verifySibling)
	if loaded.Lock.Dependencies[index].Tag != "v3.1.0" || loaded.Lock.Dependencies[index].Commit != siblingNew {
		t.Fatalf("sibling did not advance: %#v", loaded.Lock.Dependencies[index])
	}
}

func TestVerifyResumeDryRunHashesEveryFileThenApplyClearsBothSides(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	newerCommit := strings.Repeat("3", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	before := digestTree(t, root)
	remote := &verifyRemote{
		latest: map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		releases: map[string]Release{
			verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer},
		},
		commits:  map[string]string{verifySource + "@" + verifyNewer: newerCommit},
		archives: map[string][]byte{newerCommit: packageArchiveFor(t, "acme/widget", "2.2.1", "resumed\n")},
	}
	service := NewService(NewResolver(remote))

	dry, err := service.Resume(context.Background(), root, verifySource, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Changed {
		t.Fatal("resume --dry-run reported no change")
	}
	assertTreeUnchanged(t, root, before)

	applied, err := service.Resume(context.Background(), root, verifySource, false)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Changed {
		t.Fatal("resume wrote nothing")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Hold != nil || loaded.Project.Dependencies[0].Requested != "latest" {
		t.Fatalf("yaml hold survived resume: %#v", loaded.Project.Dependencies[0])
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Hold != nil || locked.Tag != verifyNewer || locked.Commit != newerCommit {
		t.Fatalf("lock hold survived resume: %#v", locked)
	}
}

func TestVerifyResumeWriteFailureRestoresBothFiles(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	projectBefore, lockBefore := readStateFiles(t, root)
	newerCommit := strings.Repeat("3", 40)
	remote := &verifyRemote{
		latest:   map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		releases: map[string]Release{verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer}},
		commits:  map[string]string{verifySource + "@" + verifyNewer: newerCommit},
		archives: map[string][]byte{newerCommit: packageArchiveFor(t, "acme/widget", "2.2.1", "resumed\n")},
	}
	planned, err := NewService(NewResolver(remote)).Resume(context.Background(), root, verifySource, true)
	if err != nil {
		t.Fatal(err)
	}
	next := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: verifySource, Requested: "latest"}}},
		Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: planned.Dependencies},
	}
	injected := errors.New("injected lock write failure")
	err = writeStateWith(root, next, func(fileRoot *os.Root, filename string, contents []byte, mode os.FileMode) (bool, error) {
		if filename == LockFilename {
			return true, injected
		}
		return true, writeFileAtomic(fileRoot, filename, contents, mode)
	})
	if err == nil || !errors.Is(err, injected) {
		t.Fatalf("writeStateWith() error = %v, want the injected failure", err)
	}
	if !strings.Contains(err.Error(), "both state files were restored") {
		t.Fatalf("write failure did not name the two-file restore: %v", err)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatalf("partial resume survived the failed lock write:\n%s\n%s", projectAfter, lockAfter)
	}
}

func TestVerifySecondRollbackAdvancesBarrierAndRetiresTheOldOne(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	newerCommit := strings.Repeat("3", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	remote := &verifyRemote{
		latest: map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		releases: map[string]Release{
			verifySource + "@" + verifyPin:   {ID: 21, Tag: verifyPin},
			verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer},
		},
		commits: map[string]string{
			verifySource + "@" + verifyPin:   pinCommit,
			verifySource + "@" + verifyNewer: newerCommit,
		},
		archives: map[string][]byte{
			pinCommit:   packageArchiveFor(t, "acme/widget", "2.1.0", "known-good\n"),
			newerCommit: packageArchiveFor(t, "acme/widget", "2.2.1", "resumed\n"),
		},
	}
	service := NewService(NewResolver(remote))
	if _, err := service.Resume(context.Background(), root, verifySource, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Install(context.Background(), root, verifySource, verifyPin, DowngradeHold, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	hold := loaded.Project.Dependencies[0].Hold
	if hold == nil || hold.Pin != verifyPin || hold.Rejected != verifyNewer {
		t.Fatalf("second rollback hold = %#v, want barrier %s", hold, verifyNewer)
	}
	if loaded.Lock.Dependencies[0].Hold == nil || loaded.Lock.Dependencies[0].Hold.RejectedTag != verifyNewer {
		t.Fatalf("second rollback lock barrier = %#v", loaded.Lock.Dependencies[0].Hold)
	}
	if beyondBarrier(Release{ID: 22, Tag: verifyBarrier}, &loaded.Lock.Dependencies[0], hold) {
		t.Fatal("retired barrier is still treated as strictly newer")
	}

	remote.latest[verifySource] = Release{ID: 22, Tag: verifyBarrier}
	result, err := service.Reconcile(context.Background(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range result.Notices {
		if strings.Contains(notice, verifyBarrier) {
			t.Fatalf("retired barrier was suggested: %q", notice)
		}
	}
}

func TestVerifyPermanentPinNeverGainsHoldAndHoldFlagIsUsageError(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("4", 40)
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: verifySource, Requested: verifyPin,
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: verifySource, Requested: verifyPin, Kind: ResolutionRelease,
			ReleaseID: 21, Tag: verifyPin, Commit: pinCommit, PackageVersion: "2.1.0",
			ContentHash: "sha256:" + strings.Repeat("4", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	projectBefore, lockBefore := readStateFiles(t, root)
	remote := &verifyRemote{
		latest:   map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		commits:  map[string]string{verifySource + "@" + verifyNewer: strings.Repeat("3", 40)},
		archives: map[string][]byte{strings.Repeat("3", 40): packageArchiveFor(t, "acme/widget", "2.2.1", "newer\n")},
	}
	service := NewService(NewResolver(remote))

	outdated, err := service.Outdated(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(outdated) != 0 {
		t.Fatalf("permanent pin appeared in outdated: %#v", outdated)
	}
	result, err := service.Update(context.Background(), root, verifySource, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || len(result.Notices) != 0 || len(result.Held) != 0 {
		t.Fatalf("update on a pin = %#v", result)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Hold != nil || loaded.Project.Dependencies[0].Requested != verifyPin {
		t.Fatalf("pin gained a hold: %#v", loaded.Project.Dependencies[0])
	}

	_, err = service.Install(context.Background(), root, verifySource, verifyBarrier, DowngradeHold, false)
	if err == nil || !strings.Contains(err.Error(), "applies only to a rollback") {
		t.Fatalf("Install(--hold) on a pin error = %v, want a usage refusal", err)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatal("--hold on a pin rewrote requested policy")
	}
}

func TestVerifyHoldMismatchShapes(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)

	t.Run("lock-only hold is a hard error", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeStateFixture(t, root,
			"schemaVersion: 2\ndependencies:\n  - source: github:acme/widget\n    requested: latest\n",
			"schemaVersion: 2\ndependencies:\n"+
				"  - source: github:acme/widget\n    requested: latest\n    kind: release\n    releaseId: 21\n    tag: v2.1.0\n    commit: "+pinCommit+"\n    packageVersion: 2.1.0\n    contentHash: sha256:"+strings.Repeat("c", 64)+"\n    hold:\n      rejectedTag: v2.2.0\n      rejectedReleaseId: 22\n",
		)
		_, err := LoadState(root)
		if err == nil {
			t.Fatal("lock-only hold loaded")
		}
		if !strings.Contains(err.Error(), "does not declare") {
			t.Fatalf("lock-only error = %v, want the yaml named as authority", err)
		}
		if !strings.Contains(err.Error(), "acr install") && !strings.Contains(err.Error(), "acr resume") {
			t.Fatalf("lock-only error = %v, want a named recovery command", err)
		}
		remote := &verifyRemote{latest: map[string]Release{verifySource: {ID: 22, Tag: verifyBarrier}}}
		_, reconcileErr := NewService(NewResolver(remote)).Reconcile(context.Background(), root, false)
		if reconcileErr == nil {
			t.Fatal("Reconcile used a lock-only hold")
		}
		if len(remote.latestCalls) != 0 || len(remote.downloadCalls) != 0 {
			t.Fatalf("lock-only hold still consulted the remote: %#v", remote)
		}
	})

	t.Run("yaml-only hold is repaired offline", func(t *testing.T) {
		t.Parallel()
		declaration := Declaration{Source: verifySource, Requested: "latest", Hold: &Hold{Pin: verifyPin, Rejected: verifyBarrier}}
		existing := &LockedDependency{
			Source: verifySource, Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 21, Tag: verifyPin, Commit: pinCommit, PackageVersion: "2.1.0",
			ContentHash: "sha256:" + strings.Repeat("c", 64),
		}
		remote := &verifyRemote{}
		decision, err := NewHoldPolicy(NewResolver(remote)).Resolve(context.Background(), declaration, existing, Release{ID: 22, Tag: verifyBarrier})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Skip || decision.Pin == nil {
			t.Fatalf("yaml-only hold = %#v, want an offline repair", decision)
		}
		if decision.Pin.Hold == nil || decision.Pin.Hold.RejectedTag != verifyBarrier {
			t.Fatalf("repaired lock hold = %#v", decision.Pin.Hold)
		}
		if decision.Pin.Tag == verifyBarrier {
			t.Fatal("repair installed the rejected release")
		}
		if len(remote.latestCalls)+len(remote.releaseCalls)+len(remote.resolveCalls)+len(remote.downloadCalls)+len(remote.assetCalls) != 0 {
			t.Fatalf("offline repair contacted the remote: %#v", remote)
		}
	})
}

func TestVerifyInstallOnAHeldDependencyNeverSilentlyRetiresTheHold(t *testing.T) {
	t.Parallel()

	pinCommit := strings.Repeat("1", 40)
	root := writeVerifyHeldProject(t, pinCommit)
	projectBefore, lockBefore := readStateFiles(t, root)
	remote := &verifyRemote{
		latest: map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		releases: map[string]Release{
			verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer},
		},
		commits: map[string]string{
			verifySource + "@" + verifyBarrier: strings.Repeat("2", 40),
			verifySource + "@" + verifyNewer:   strings.Repeat("3", 40),
		},
		archives: map[string][]byte{
			strings.Repeat("2", 40): packageArchiveFor(t, "acme/widget", "2.2.0", "rejected\n"),
			strings.Repeat("3", 40): packageArchiveFor(t, "acme/widget", "2.2.1", "newer\n"),
		},
	}
	service := NewService(NewResolver(remote))

	result, err := service.Install(context.Background(), root, verifySource, "latest", DowngradeUnset, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Hold == nil {
		t.Fatalf("install SOURCE retired the hold: %#v result=%#v", loaded.Project.Dependencies[0], result)
	}
	if loaded.Project.Dependencies[0].Requested != "latest" {
		t.Fatalf("install SOURCE rewrote requested: %#v", loaded.Project.Dependencies[0])
	}

	_, err = service.Install(context.Background(), root, verifySource, verifyNewer, DowngradeUnset, false)
	var required *DowngradeRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("install SOURCE@%s error = %v, want a required choice", verifyNewer, err)
	}
	if !strings.Contains(err.Error(), "--hold") || !strings.Contains(err.Error(), "--pin") {
		t.Fatalf("choice error = %v, want both flags named", err)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatal("a refused install over a hold still wrote state")
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, NewApplication(remote), "test").Run(context.Background(), []string{
		"install", verifySource + "@" + verifyNewer, "--project", root, "--json",
	})
	if exitCode != cli.ExitUsage {
		t.Fatalf("install SOURCE@newer exit = %d, want 2, stderr = %q", exitCode, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("usage error leaked onto stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "downgrade_choice_required") {
		t.Fatalf("stderr = %q, want downgrade_choice_required", stderr.String())
	}
}

func TestVerifyHoldFlagWithACommitSHADoesNotBecomeAPermanentPin(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	lockedCommit := strings.Repeat("5", 40)
	sha := strings.Repeat("ab", 20)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: verifySource, Requested: "latest",
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: verifySource, Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 23, Tag: verifyNewer, Commit: lockedCommit, PackageVersion: "2.2.1",
			ContentHash: "sha256:" + strings.Repeat("5", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := &verifyRemote{
		latest: map[string]Release{verifySource: {ID: 23, Tag: verifyNewer}},
		releases: map[string]Release{
			verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer},
		},
		commits: map[string]string{
			verifySource + "@" + sha:         sha,
			verifySource + "@" + verifyNewer: lockedCommit,
		},
		archives: map[string][]byte{
			sha:          packageArchiveFor(t, "acme/widget", "2.1.0", "sha-pin\n"),
			lockedCommit: packageArchiveFor(t, "acme/widget", "2.2.1", "current\n"),
		},
	}

	_, err := NewService(NewResolver(remote)).Install(context.Background(), root, verifySource, sha, DowngradeHold, false)
	if err != nil {
		t.Fatalf("Install(--hold SHA) error = %v; the issue AC allows a SHA as a temporary rollback pin", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	declaration := loaded.Project.Dependencies[0]
	if declaration.Requested != "latest" {
		t.Fatalf("--hold SHA converted latest into a permanent pin: %#v", declaration)
	}
	if declaration.Hold == nil || declaration.Hold.Pin != sha || declaration.Hold.Rejected != verifyNewer {
		t.Fatalf("--hold SHA hold = %#v, want pin %s barrier %s", declaration.Hold, sha, verifyNewer)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Kind != ResolutionCommit || locked.Commit != sha || locked.Hold == nil || locked.Hold.RejectedTag != verifyNewer {
		t.Fatalf("--hold SHA lock = %#v, want a held commit at the SHA", locked)
	}
}

func TestVerifySchemaVersionOneLoadsWithoutRewriteOrInventedHolds(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pinCommit := strings.Repeat("a", 40)
	latestCommit := strings.Repeat("b", 40)
	project := "" +
		"schemaVersion: 1\n" +
		"dependencies:\n" +
		"  - source: github:acme/widget\n" +
		"    requested: latest\n" +
		"  - source: github:acme/pinned\n" +
		"    requested: v9.9.9\n"
	lock := "" +
		"schemaVersion: 1\n" +
		"dependencies:\n" +
		"  - source: github:acme/widget\n" +
		"    requested: latest\n" +
		"    kind: release\n" +
		"    releaseId: 1\n" +
		"    tag: v1.0.0\n" +
		"    commit: " + latestCommit + "\n" +
		"    packageVersion: 1.0.0\n" +
		"    contentHash: sha256:" + strings.Repeat("b", 64) + "\n" +
		"  - source: github:acme/pinned\n" +
		"    requested: v9.9.9\n" +
		"    kind: release\n" +
		"    releaseId: 9\n" +
		"    tag: v9.9.9\n" +
		"    commit: " + pinCommit + "\n" +
		"    packageVersion: 9.9.9\n" +
		"    contentHash: sha256:" + strings.Repeat("a", 64) + "\n"
	writeStateFixture(t, root, project, lock)

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range loaded.Project.Dependencies {
		if declaration.Hold != nil {
			t.Fatalf("schema 1 load invented a hold for %s: %#v", declaration.Source, declaration.Hold)
		}
	}
	index, _ := findDeclaration(loaded.Project.Dependencies, "github:acme/pinned")
	if loaded.Project.Dependencies[index].Requested != "v9.9.9" {
		t.Fatalf("schema 1 load pin-ified a pin: %#v", loaded.Project.Dependencies[index])
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != project {
		t.Fatalf("LoadState rewrote agents.yaml:\n%s", got)
	}
	if got := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename))); got != lock {
		t.Fatalf("LoadState rewrote the lock:\n%s", got)
	}

	statuses, err := NewService(NewResolver(&verifyRemote{})).List(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != project {
		t.Fatalf("list rewrote schema-1 yaml:\n%s", got)
	}
	if len(statuses) != 2 {
		t.Fatalf("list = %#v", statuses)
	}
}

func TestVerifyJSONEnvelopesDistinguishHeldFromOrdinaryOnStdout(t *testing.T) {
	t.Parallel()

	heldCommit := strings.Repeat("1", 40)
	ordinaryOld := strings.Repeat("8", 40)
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: verifySource, Requested: "latest", Hold: &Hold{Pin: verifyPin, Rejected: verifyBarrier}},
			{Source: verifySibling, Requested: "latest"},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{
				Source: verifySource, Requested: "latest", Kind: ResolutionRelease,
				ReleaseID: 21, Tag: verifyPin, Commit: heldCommit, PackageVersion: "2.1.0",
				ContentHash: "sha256:" + strings.Repeat("c", 64),
				Hold:        &LockHold{RejectedTag: verifyBarrier, RejectedReleaseID: 22},
			},
			{
				Source: verifySibling, Requested: "latest", Kind: ResolutionRelease,
				ReleaseID: 80, Tag: "v3.0.0", Commit: ordinaryOld, PackageVersion: "3.0.0",
				ContentHash: "sha256:" + strings.Repeat("8", 64),
			},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("json dry-run must not touch me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &verifyRemote{
		latest: map[string]Release{
			verifySource:  {ID: 23, Tag: verifyNewer},
			verifySibling: {ID: 81, Tag: "v3.1.0"},
		},
		releases: map[string]Release{verifySource + "@" + verifyNewer: {ID: 23, Tag: verifyNewer}},
		commits: map[string]string{
			verifySource + "@" + verifyNewer: strings.Repeat("3", 40),
			verifySibling + "@v3.1.0":        strings.Repeat("9", 40),
		},
		archives: map[string][]byte{
			strings.Repeat("3", 40): packageArchiveFor(t, "acme/widget", "2.2.1", "resumed\n"),
		},
	}
	app := NewApplication(remote)

	t.Run("list", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := cli.New(&stdout, &stderr, app, "test").Run(context.Background(), []string{"list", "--project", root, "--json"})
		if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("list --json exit = %d stderr = %q", exitCode, stderr.String())
		}
		var envelope struct {
			OK     bool `json:"ok"`
			Result struct {
				Dependencies []struct {
					Declaration struct {
						Source string `json:"source"`
						Hold   *struct {
							Pin      string `json:"pin"`
							Rejected string `json:"rejected"`
						} `json:"hold"`
					} `json:"declaration"`
					Locked *struct {
						Hold *struct {
							RejectedTag string `json:"rejectedTag"`
						} `json:"hold"`
					} `json:"locked"`
				} `json:"dependencies"`
			} `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("list envelope %q: %v", stdout.String(), err)
		}
		if !envelope.OK || len(envelope.Result.Dependencies) != 2 {
			t.Fatalf("list envelope = %#v", envelope)
		}
		var sawHeld, sawOrdinary bool
		for _, row := range envelope.Result.Dependencies {
			switch row.Declaration.Source {
			case verifySource:
				if row.Declaration.Hold == nil || row.Declaration.Hold.Rejected != verifyBarrier || row.Locked == nil || row.Locked.Hold == nil {
					t.Fatalf("held list row = %#v", row)
				}
				sawHeld = true
			case verifySibling:
				if row.Declaration.Hold != nil || (row.Locked != nil && row.Locked.Hold != nil) {
					t.Fatalf("ordinary list row carried a hold: %#v", row)
				}
				sawOrdinary = true
			}
		}
		if !sawHeld || !sawOrdinary {
			t.Fatalf("list JSON did not type held vs ordinary: %#v", envelope.Result.Dependencies)
		}
	})

	t.Run("outdated", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := cli.New(&stdout, &stderr, app, "test").Run(context.Background(), []string{"outdated", "--project", root, "--json"})
		if exitCode != cli.ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("outdated --json exit = %d stderr = %q stdout = %q", exitCode, stderr.String(), stdout.String())
		}
		var envelope struct {
			OK     bool `json:"ok"`
			Result struct {
				Outdated []struct {
					Source        string `json:"source"`
					Status        string `json:"status"`
					ResumeCommand string `json:"resumeCommand"`
					Hold          *struct {
						Rejected string `json:"rejected"`
					} `json:"hold"`
				} `json:"outdated"`
			} `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("outdated envelope %q: %v", stdout.String(), err)
		}
		if !envelope.OK {
			t.Fatalf("outdated envelope not ok: %s", stdout.String())
		}
		bySource := map[string]string{}
		for _, row := range envelope.Result.Outdated {
			bySource[row.Source] = row.Status
			switch row.Status {
			case "update":
				if row.Hold != nil || row.ResumeCommand != "" {
					t.Fatalf("ordinary outdated row = %#v", row)
				}
			case "beyond-barrier":
				if row.Hold == nil || row.ResumeCommand != "acr resume "+verifySource {
					t.Fatalf("beyond-barrier row = %#v", row)
				}
			case "held":
				if row.Hold == nil || row.ResumeCommand != "" {
					t.Fatalf("held row = %#v", row)
				}
			default:
				t.Fatalf("untyped outdated row = %#v", row)
			}
		}
		if bySource[verifySibling] != "update" {
			t.Fatalf("ordinary sibling status = %q", bySource[verifySibling])
		}
		if bySource[verifySource] != "beyond-barrier" && bySource[verifySource] != "held" {
			t.Fatalf("held source status = %q, want a typed hold status", bySource[verifySource])
		}
	})

	t.Run("resume-dry-run", func(t *testing.T) {
		before := digestTree(t, root)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := cli.New(&stdout, &stderr, app, "test").Run(context.Background(), []string{
			"resume", verifySource, "--project", root, "--dry-run", "--json",
		})
		if exitCode != cli.ExitSuccess {
			t.Fatalf("resume --dry-run --json exit = %d stderr = %q", exitCode, stderr.String())
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("resume envelope %q: %v", stdout.String(), err)
		}
		if string(envelope["ok"]) != "true" || !bytes.Contains(envelope["command"], []byte("resume")) {
			t.Fatalf("resume envelope = %s", stdout.String())
		}
		assertTreeUnchanged(t, root, before)
	})
}

func TestVerifyResolveAtChecksReleaseMetadataWhileCommitPinSkipsIt(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	tag := "v4.5.6"
	archive := packageArchiveFor(t, "acme/widget", "4.5.6", "payload\n")
	matching := releaseMetadataJSON(1, commit, contentHashOf(t, archive, "acme/widget"))

	t.Run("release", func(t *testing.T) {
		t.Parallel()
		remote := &verifyRemote{
			releases: map[string]Release{verifySource + "@" + tag: {
				ID: 45, Tag: tag, Assets: []ReleaseAsset{{ID: 7, Name: ReleaseMetadataAssetName}},
			}},
			commits:  map[string]string{verifySource + "@" + tag: commit},
			archives: map[string][]byte{commit: archive},
			assets:   map[int64][]byte{7: matching},
		}
		resolver := NewResolver(remote)
		declaration := Declaration{Source: verifySource, Requested: tag}
		candidate, err := resolver.Candidate(context.Background(), declaration)
		if err != nil {
			t.Fatal(err)
		}
		locked, err := resolver.ResolveAt(context.Background(), declaration, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if locked.Tag != tag || len(remote.assetCalls) != 1 {
			t.Fatalf("release ResolveAt = %#v assetCalls=%v", locked, remote.assetCalls)
		}
	})

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		remote := &verifyRemote{
			releases: map[string]Release{verifySource + "@" + tag: {
				ID: 45, Tag: tag, Assets: []ReleaseAsset{{ID: 7, Name: ReleaseMetadataAssetName}},
			}},
			commits:  map[string]string{verifySource + "@" + commit: commit},
			archives: map[string][]byte{commit: archive},
			assets:   map[int64][]byte{7: []byte(`{"metadataVersion":1,"commit":"deadbeef","contentHash":"sha256:` + strings.Repeat("0", 64) + `"}`)},
		}
		resolver := NewResolver(remote)
		declaration := Declaration{Source: verifySource, Requested: commit}
		candidate, err := resolver.Candidate(context.Background(), declaration)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(candidate, Release{}) {
			t.Fatalf("commit Candidate() = %#v, want zero release", candidate)
		}
		locked, err := resolver.ResolveAt(context.Background(), declaration, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if locked.Kind != ResolutionCommit || len(remote.assetCalls) != 0 || len(remote.releaseCalls) != 0 || len(remote.latestCalls) != 0 {
			t.Fatalf("commit pin consulted release metadata: locked=%#v remote=%#v", locked, remote)
		}
	})
}

func lockRow(t *testing.T, root, source string) string {
	t.Helper()
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	index, exists := findLock(loaded.Lock.Dependencies, source)
	if !exists {
		t.Fatalf("no lock for %s", source)
	}
	encoded, err := json.Marshal(loaded.Lock.Dependencies[index])
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func contentHashOf(t *testing.T, archive []byte, fullName string) string {
	t.Helper()
	repository, err := ParseSource("github:" + fullName)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyPackageArchive(archive, repository)
	if err != nil {
		t.Fatal(err)
	}
	return verified.ContentHash
}
