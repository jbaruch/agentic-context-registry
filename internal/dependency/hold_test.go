package dependency

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func heldState(pin, rejected string) State {
	return State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: "github:owner/plugin", Requested: "latest",
			Hold: &Hold{Pin: pin, Rejected: rejected, Reason: rejected + " breaks the review hook"},
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease,
			ReleaseID: 987, Tag: pin, Commit: strings.Repeat("a", 40), PackageVersion: strings.TrimPrefix(pin, "v"),
			ContentHash: "sha256:" + strings.Repeat("b", 64),
			Hold:        &LockHold{RejectedTag: rejected, RejectedReleaseID: 1024, RejectedCommit: strings.Repeat("c", 40)},
		}}},
	}
}

func TestHoldRoundTripsThroughProjectState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := WriteState(root, heldState("v1.3.2", "v1.4.0")); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	project := readTestFile(t, filepath.Join(root, ProjectFilename))
	if !strings.Contains(project, "requested: latest") || !strings.Contains(project, "pin: v1.3.2") || !strings.Contains(project, "rejected: v1.4.0") {
		t.Fatalf("agents.yaml does not record reviewable hold intent:\n%s", project)
	}

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	hold := loaded.Project.Dependencies[0].Hold
	if hold == nil || hold.Pin != "v1.3.2" || hold.Rejected != "v1.4.0" || hold.Reason != "v1.4.0 breaks the review hook" {
		t.Fatalf("loaded hold = %#v", hold)
	}
	if loaded.Project.Dependencies[0].Requested != "latest" {
		t.Fatalf("hold rewrote requested to %q", loaded.Project.Dependencies[0].Requested)
	}
	if err := WriteState(root, loaded); err != nil {
		t.Fatalf("rewrite error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != project {
		t.Fatalf("agents.yaml round trip is not byte stable:\n%s\nwant:\n%s", got, project)
	}
}

func TestHeldLockRecordsHeldRelease(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := heldState("v1.3.2", "v1.4.0")
	if err := WriteState(root, state); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	locked := loaded.Lock.Dependencies[0]
	if locked.Tag != "v1.3.2" || locked.Commit != strings.Repeat("a", 40) || locked.PackageVersion != "1.3.2" || locked.ContentHash != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("lock does not record the known-good release: %#v", locked)
	}
	if locked.Hold == nil || locked.Hold.RejectedTag != "v1.4.0" || locked.Hold.RejectedReleaseID != 1024 || locked.Hold.RejectedCommit != strings.Repeat("c", 40) {
		t.Fatalf("lock does not record the rejected identity: %#v", locked.Hold)
	}
}

func TestHeldCommitPinCoexistsWithLatestRequest(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("d", 40)
	state := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
			Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: commit, Rejected: "v1.4.0"},
		}}},
		Lock: Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionCommit, Commit: commit,
			PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("e", 64),
			Hold: &LockHold{RejectedTag: "v1.4.0"},
		}}},
	}
	if err := validateState(state.Project, state.Lock); err != nil {
		t.Fatalf("validateState() rejected a held commit pin: %v", err)
	}
	lockSchema := compileDependencySchema(t, "registry-lock.schema.json")
	if err := validateDependencySchema(t, lockSchema, state.Lock); err != nil {
		t.Fatalf("lock schema rejected a held commit pin: %v", err)
	}
}

func TestHoldValidationRejectsUnsafeRecords(t *testing.T) {
	t.Parallel()

	projectSchema := compileDependencySchema(t, "agents.schema.json")
	tests := []struct {
		name string
		// schemaExpressible is false for invariants JSON Schema cannot state,
		// which the runtime still enforces.
		schemaExpressible bool
		declaration       Declaration
		wantMessage       string
	}{
		{
			name:              "hold on a fixed request",
			schemaExpressible: true,
			declaration:       Declaration{Source: "github:owner/plugin", Requested: "v1.3.2", Hold: &Hold{Pin: "v1.3.2", Rejected: "v1.4.0"}},
			wantMessage:       "only valid on a latest declaration",
		},
		{
			name:              "pin is latest",
			schemaExpressible: true,
			declaration:       Declaration{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "latest", Rejected: "v1.4.0"}},
			wantMessage:       "hold.pin must name a fixed release tag or commit",
		},
		{
			name:              "rejected is latest",
			schemaExpressible: true,
			declaration:       Declaration{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "v1.3.2", Rejected: "latest"}},
			wantMessage:       "hold.rejected must name a fixed release tag or commit",
		},
		{
			name:              "rejected is a commit",
			schemaExpressible: true,
			declaration:       Declaration{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "v1.3.2", Rejected: strings.Repeat("f", 40)}},
			wantMessage:       "hold.rejected must be a release tag",
		},
		{
			name:        "pin equals rejected",
			declaration: Declaration{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "v1.4.0", Rejected: "v1.4.0"}},
			wantMessage: "must pin a different reference",
		},
		{
			name:              "invalid pin reference",
			schemaExpressible: true,
			declaration:       Declaration{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "bad@tag", Rejected: "v1.4.0"}},
			wantMessage:       "hold.pin:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{test.declaration}}
			err := validateState(project, Lockfile{SchemaVersion: CurrentSchemaVersion})
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("validateState() error = %v, want %q", err, test.wantMessage)
			}
			schemaRejected := validateDependencySchema(t, projectSchema, project) != nil
			if schemaRejected != test.schemaExpressible {
				t.Fatalf("agents schema rejected %s = %t, want %t", test.name, schemaRejected, test.schemaExpressible)
			}
		})
	}
}

func TestLockHoldMustMatchDeclaredBarrier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		declared    *Hold
		locked      *LockHold
		wantMessage string
	}{
		{
			name:        "lock barrier the project does not declare",
			locked:      &LockHold{RejectedTag: "v1.4.0"},
			wantMessage: "does not declare",
		},
		{
			name:        "barriers disagree",
			declared:    &Hold{Pin: "v1.3.2", Rejected: "v1.4.0"},
			locked:      &LockHold{RejectedTag: "v1.5.0"},
			wantMessage: "delete this dependency's entry from",
		},
		{
			name:        "rejected commit is not a commit",
			declared:    &Hold{Pin: "v1.3.2", Rejected: "v1.4.0"},
			locked:      &LockHold{RejectedTag: "v1.4.0", RejectedCommit: "not-a-commit"},
			wantMessage: "delete the rejectedCommit line",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pin := "v1.3.2"
			if test.declared == nil {
				pin = "latest"
			}
			project := Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{
				Source: "github:owner/plugin", Requested: "latest", Hold: test.declared,
			}}}
			lock := Lockfile{SchemaVersion: CurrentSchemaVersion, Dependencies: []LockedDependency{{
				Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1,
				Tag: strings.TrimPrefix(pin, "latest"), Commit: strings.Repeat("a", 40),
				PackageVersion: "1.3.2", ContentHash: "sha256:" + strings.Repeat("b", 64), Hold: test.locked,
			}}}
			if test.declared == nil {
				lock.Dependencies[0].Tag = "v1.3.2"
			}
			err := validateState(project, lock)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("validateState() error = %v, want %q", err, test.wantMessage)
			}
		})
	}
}

func TestLoadStateUpgradesSchemaVersionOneWithoutRewriting(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	project := "schemaVersion: 1\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n  - source: github:owner/pinned\n    requested: v1.2.3\n"
	lock := "schemaVersion: 1\ndependencies:\n" +
		"  - source: github:owner/pinned\n    requested: v1.2.3\n    kind: release\n    releaseId: 7\n    tag: v1.2.3\n    commit: " + commit + "\n    packageVersion: 1.2.3\n    contentHash: sha256:" + strings.Repeat("b", 64) + "\n"
	writeStateFixture(t, root, project, lock)

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if loaded.Project.SchemaVersion != CurrentSchemaVersion || loaded.Lock.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("LoadState() versions = %d/%d, want %d", loaded.Project.SchemaVersion, loaded.Lock.SchemaVersion, CurrentSchemaVersion)
	}
	for _, declaration := range loaded.Project.Dependencies {
		if declaration.Hold != nil {
			t.Fatalf("migration invented a hold for %s: %#v", declaration.Source, declaration.Hold)
		}
	}
	if index, _ := findDeclaration(loaded.Project.Dependencies, "github:owner/pinned"); loaded.Project.Dependencies[index].Requested != "v1.2.3" {
		t.Fatalf("migration changed a permanent pin: %#v", loaded.Project.Dependencies[index])
	}
	if got := readTestFile(t, filepath.Join(root, ProjectFilename)); got != project {
		t.Fatalf("LoadState() rewrote agents.yaml:\n%s", got)
	}
	if got := readTestFile(t, filepath.Join(root, filepath.FromSlash(LockFilename))); got != lock {
		t.Fatalf("LoadState() rewrote the lockfile:\n%s", got)
	}
}

func TestLoadStateRejectsUnsupportedSchemaVersions(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"0", "3", "99"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			writeStateFixture(t, root, "schemaVersion: "+version+"\n", "schemaVersion: 2\n")
			_, err := LoadState(root)
			if err == nil || !strings.Contains(err.Error(), "unsupported agents.yaml schemaVersion "+version) {
				t.Fatalf("LoadState() error = %v, want unsupported version %s", err, version)
			}
		})
	}
}

func TestWriteStateRejectsSupersededSchemaVersion(t *testing.T) {
	t.Parallel()

	state := heldState("v1.3.2", "v1.4.0")
	state.Project.SchemaVersion = MinimumSchemaVersion
	err := WriteState(t.TempDir(), state)
	if err == nil || !strings.Contains(err.Error(), "unsupported agents.yaml schemaVersion 1") {
		t.Fatalf("WriteState() error = %v, want a refusal to write the superseded version", err)
	}
}

// Every command that could repair a lock-hold disagreement validates this state
// before it runs, so each recovery the diagnostic names must be an edit that
// loads on its own.
func TestLockHoldRecoveryInstructionsLoad(t *testing.T) {
	t.Parallel()

	const (
		declaredBarrier = "schemaVersion: 2\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n    hold:\n      pin: v1.3.2\n      rejected: v1.4.0\n"
		lockedBarrier   = "schemaVersion: 2\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n    kind: release\n    releaseId: 987\n    tag: v1.3.2\n    commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    packageVersion: 1.3.2\n    contentHash: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n    hold:\n      rejectedTag: "
	)
	tests := []struct {
		name          string
		brokenProject string
		brokenLock    string
		project       string
		lock          string
	}{
		{
			name:          "delete the lock entry",
			brokenProject: declaredBarrier,
			brokenLock:    lockedBarrier + "v1.5.0\n",
			project:       declaredBarrier,
			lock:          "schemaVersion: 2\ndependencies: []\n",
		},
		{
			name:          "name the locked barrier in the declaration",
			brokenProject: declaredBarrier,
			brokenLock:    lockedBarrier + "v1.5.0\n",
			project:       strings.Replace(declaredBarrier, "rejected: v1.4.0", "rejected: v1.5.0", 1),
			lock:          lockedBarrier + "v1.5.0\n",
		},
		{
			name:          "delete an invalid rejected commit",
			brokenProject: declaredBarrier,
			brokenLock:    lockedBarrier + "v1.4.0\n      rejectedCommit: not-a-commit\n",
			project:       declaredBarrier,
			lock:          lockedBarrier + "v1.4.0\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			broken := t.TempDir()
			writeStateFixture(t, broken, test.brokenProject, test.brokenLock)
			if _, err := LoadState(broken); err == nil {
				t.Fatal("LoadState() accepted the state the recovery is written for")
			}

			repaired := t.TempDir()
			writeStateFixture(t, repaired, test.project, test.lock)
			if _, err := LoadState(repaired); err != nil {
				t.Fatalf("LoadState() after the named recovery = %v, want the state to load", err)
			}
		})
	}
}

// A commit-SHA barrier is refused while agents.yaml loads, so the recovery the
// diagnostic names must be an edit that makes the same state load.
func TestCommitBarrierRecoveryInstructionLoads(t *testing.T) {
	t.Parallel()

	const lock = "schemaVersion: 2\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n    kind: release\n    releaseId: 987\n    tag: v1.3.2\n    commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    packageVersion: 1.3.2\n    contentHash: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	held := func(rejected string) string {
		return "schemaVersion: 2\ndependencies:\n  - source: github:owner/plugin\n    requested: latest\n    hold:\n      pin: v1.3.2\n      rejected: " + rejected + "\n"
	}

	broken := t.TempDir()
	writeStateFixture(t, broken, held(strings.Repeat("f", 40)), lock)
	_, err := LoadState(broken)
	if err == nil {
		t.Fatal("LoadState() accepted a commit SHA as the resume barrier")
	}
	if strings.Contains(err.Error(), "acr resume") {
		t.Fatalf("commit-barrier error named acr resume, which cannot run before the state loads: %v", err)
	}
	if !strings.Contains(err.Error(), "edit "+ProjectFilename) {
		t.Fatalf("commit-barrier error = %v, want the pre-load edit named", err)
	}

	repaired := t.TempDir()
	writeStateFixture(t, repaired, held("v1.4.0"), lock)
	if _, err := LoadState(repaired); err != nil {
		t.Fatalf("LoadState() after the named recovery = %v, want the state to load", err)
	}
}

func writeStateFixture(t *testing.T, root, project, lock string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ProjectFilename), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(LockFilename)), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}
