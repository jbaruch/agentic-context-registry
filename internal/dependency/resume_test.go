package dependency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// resumeRemote answers for a repository whose latest release is beyond the
// barrier, with the held release still resolvable.
func resumeRemote(t *testing.T, newerTag, newerCommit string) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{
		latest:   Release{ID: 2048, Tag: newerTag},
		releases: map[string]Release{heldTag: {ID: 987, Tag: heldTag}, rejectedTag: {ID: 1024, Tag: rejectedTag}},
		commits:  map[string]string{heldTag: strings.Repeat("a", 40), newerTag: newerCommit},
		archives: map[string][]byte{newerCommit: packageArchive(t, strings.TrimPrefix(newerTag, "v"), "resumed\n")},
	}
}

// hashProjectFiles digests every file under root so a dry run can be proved to
// have written nothing at all, not merely nothing to the two state files.
func hashProjectFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
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

func TestResumeDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	before := hashProjectFiles(t, root)
	remote := resumeRemote(t, "v1.4.1", strings.Repeat("e", 40))

	result, err := NewService(NewResolver(remote)).Resume(context.Background(), root, heldSource, true)
	if err != nil {
		t.Fatalf("Resume(--dry-run) error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Resume(--dry-run) Changed = false, want the planned resume reported")
	}
	if index, _ := findLock(result.Dependencies, heldSource); result.Dependencies[index].Tag != "v1.4.1" {
		t.Fatalf("Resume(--dry-run) planned lock = %#v", result.Dependencies[index])
	}
	after := hashProjectFiles(t, root)
	if len(after) != len(before) {
		t.Fatalf("Resume(--dry-run) changed the file set: %v -> %v", sortedKeys(before), sortedKeys(after))
	}
	for name, digest := range before {
		if after[name] != digest {
			t.Fatalf("Resume(--dry-run) rewrote %s", name)
		}
	}
}

func TestResumeClearsTheHoldTransactionally(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	newerCommit := strings.Repeat("e", 40)
	remote := resumeRemote(t, "v1.4.1", newerCommit)

	result, err := NewService(NewResolver(remote)).Resume(context.Background(), root, heldSource, false)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !result.Changed {
		t.Fatal("Resume() Changed = false")
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Dependencies[0].Hold != nil || loaded.Project.Dependencies[0].Requested != "latest" {
		t.Fatalf("resumed declaration = %#v", loaded.Project.Dependencies[0])
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Hold != nil || locked.Tag != "v1.4.1" || locked.Commit != newerCommit {
		t.Fatalf("resumed lock = %#v", locked)
	}
	if remote.downloadCalls != 1 {
		t.Fatalf("Resume() downloads = %d, want only the resumed release", remote.downloadCalls)
	}
}

func TestResumeRollsBackOnLockWriteFailure(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	projectBefore, lockBefore := readStateFiles(t, root)
	remote := resumeRemote(t, "v1.4.1", strings.Repeat("e", 40))

	result, err := NewService(NewResolver(remote)).Resume(context.Background(), root, heldSource, true)
	if err != nil {
		t.Fatalf("Resume(--dry-run) error = %v", err)
	}
	resumed := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: heldSource, Requested: "latest"}}},
		Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: result.Dependencies},
	}
	injected := errors.New("lock replacement failed")
	err = writeStateWith(root, resumed, func(fileRoot *os.Root, filename string, contents []byte, mode os.FileMode) (bool, error) {
		if filename == LockFilename {
			return true, injected
		}
		return true, writeFileAtomic(fileRoot, filename, contents, mode)
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "both state files were restored") {
		t.Fatalf("writeStateWith() error = %v, want a restored-state diagnostic", err)
	}
	projectAfter, lockAfter := readStateFiles(t, root)
	if projectAfter != projectBefore || lockAfter != lockBefore {
		t.Fatalf("a failed resume left partial state:\n%s\n%s", projectAfter, lockAfter)
	}
}

func TestResumeLeavesOtherDependenciesByteIdentical(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pinnedCommit := strings.Repeat("5", 40)
	siblingCommit := strings.Repeat("1", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{
			{Source: heldSource, Requested: "latest", Hold: &Hold{Pin: heldTag, Rejected: rejectedTag}},
			{Source: siblingSourc, Requested: "latest"},
			{Source: "github:owner/pinned", Requested: pinnedCommit[:12]},
		}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{
			{Source: heldSource, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 987, Tag: heldTag,
				Commit: strings.Repeat("a", 40), PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64),
				Hold: &LockHold{RejectedTag: rejectedTag, RejectedReleaseID: 1024}},
			{Source: siblingSourc, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 5, Tag: "v2.0.0",
				Commit: siblingCommit, PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64)},
			{Source: "github:owner/pinned", Requested: pinnedCommit[:12], Kind: ResolutionCommit, Commit: pinnedCommit,
				PackageVersion: "9.9.9", ContentHash: "sha256:" + strings.Repeat("4", 64)},
		}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	resumedCommit := strings.Repeat("e", 40)
	remote := &perSourceGitHub{
		latest:   map[string]Release{heldSource: {ID: 2048, Tag: "v1.4.1"}, siblingSourc: {ID: 5, Tag: "v2.0.0"}},
		commits:  map[string]string{heldSource + "@v1.4.1": resumedCommit, siblingSourc + "@v2.0.0": siblingCommit},
		archives: map[string][]byte{resumedCommit: packageArchive(t, "1.4.1", "resumed\n")},
	}

	if _, err := NewService(NewResolver(remote)).Resume(context.Background(), root, heldSource, false); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{siblingSourc, "github:owner/pinned"} {
		index, _ := findLock(loaded.Lock.Dependencies, source)
		original, _ := findLock(state.Lock.Dependencies, source)
		if !reflect.DeepEqual(loaded.Lock.Dependencies[index], state.Lock.Dependencies[original]) {
			t.Fatalf("resume changed unrelated %s:\n%#v\n%#v", source, loaded.Lock.Dependencies[index], state.Lock.Dependencies[original])
		}
	}
	if remote.downloadCalls != 1 {
		t.Fatalf("Resume() downloads = %d, want only the resumed dependency", remote.downloadCalls)
	}
}

func TestSecondRollbackAdvancesBarrier(t *testing.T) {
	t.Parallel()

	root := heldProject(t, strings.Repeat("a", 40))
	resumedCommit := strings.Repeat("e", 40)
	heldCommit := strings.Repeat("a", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 2048, Tag: "v1.4.1"},
		releases: map[string]Release{heldTag: {ID: 987, Tag: heldTag}, "v1.4.1": {ID: 2048, Tag: "v1.4.1"}},
		commits:  map[string]string{heldTag: heldCommit, "v1.4.1": resumedCommit},
		archives: map[string][]byte{
			resumedCommit: packageArchive(t, "1.4.1", "resumed\n"),
			heldCommit:    packageArchive(t, "1.3.2", "known good\n"),
		},
	}
	service := NewService(NewResolver(remote))

	if _, err := service.Resume(context.Background(), root, heldSource, false); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if _, err := service.Install(context.Background(), root, heldSource, heldTag, DowngradeHold, false); err != nil {
		t.Fatalf("Install(--hold) after resume error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	hold := loaded.Project.Dependencies[0].Hold
	if hold == nil || hold.Pin != heldTag || hold.Rejected != "v1.4.1" {
		t.Fatalf("second rollback hold = %#v, want the barrier advanced to v1.4.1", hold)
	}
	if loaded.Lock.Dependencies[0].Hold == nil || loaded.Lock.Dependencies[0].Hold.RejectedTag != "v1.4.1" {
		t.Fatalf("second rollback lock barrier = %#v", loaded.Lock.Dependencies[0].Hold)
	}
	// The retired barrier can never be suggested again: it is not strictly
	// newer than the barrier that replaced it.
	if beyondBarrier(Release{ID: 1024, Tag: rejectedTag}, &loaded.Lock.Dependencies[0], hold) {
		t.Fatal("the retired barrier is suggestible again")
	}
}

func TestResumeRejectsSourcesWithoutAHold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		source      string
		wantMessage string
	}{
		{name: "undeclared", source: "github:owner/missing", wantMessage: "is not declared"},
		{name: "declared without a hold", source: siblingSourc, wantMessage: "has no rollback hold; nothing to resume"},
	}
	root := t.TempDir()
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: siblingSourc, Requested: "latest"}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: siblingSourc, Requested: "latest", Kind: ResolutionRelease, ReleaseID: 5, Tag: "v2.0.0",
			Commit: strings.Repeat("1", 40), PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("3", 64),
		}}},
	}
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := &fakeGitHub{err: errors.New("remote must not be called")}
			_, err := NewService(NewResolver(remote)).Resume(context.Background(), root, test.source, false)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Resume(%s) error = %v, want %q", test.source, err, test.wantMessage)
			}
		})
	}
}

func TestResumeUsageAndJSONEnvelope(t *testing.T) {
	t.Parallel()

	t.Run("missing source", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), "test").Run(context.Background(), []string{"resume"})
		if exitCode != cli.ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "acr resume SOURCE") {
			t.Fatalf("Run(resume) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})

	t.Run("dry run envelope", func(t *testing.T) {
		root := heldProject(t, strings.Repeat("a", 40))
		before := hashProjectFiles(t, root)
		remote := resumeRemote(t, "v1.4.1", strings.Repeat("e", 40))
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		exitCode := cli.New(&stdout, &stderr, NewApplication(remote), "test").Run(context.Background(), []string{"resume", heldSource, "--project", root, "--dry-run", "--json"})

		if exitCode != cli.ExitSuccess {
			t.Fatalf("Run(resume --dry-run --json) exit = %d, stderr = %q", exitCode, stderr.String())
		}
		var envelope struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Result  struct {
				Changed bool `json:"changed"`
			} `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		if !envelope.OK || envelope.Command != "resume" || !envelope.Result.Changed {
			t.Fatalf("resume envelope = %#v", envelope)
		}
		for name, digest := range before {
			if hashProjectFiles(t, root)[name] != digest {
				t.Fatalf("resume --dry-run rewrote %s", name)
			}
		}
	})
}

func sortedKeys(digests map[string]string) []string {
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
