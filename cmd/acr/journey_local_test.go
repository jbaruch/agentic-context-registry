package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestLocalBuiltCLIJourney(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	pkg := newJourneyPackage(t, "local/example", "1.0.0")
	source := filepath.Join(t.TempDir(), "plugin@dev")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("undeclared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalSource := snapshotProjectTree(t, source)
	pathFromProject, err := filepath.Rel(project.root, source)
	if err != nil {
		t.Fatal(err)
	}
	project.runBinary(binary, 0, "install", "file:"+pathFromProject, "--agent", "codex", "--agent", "claude-code", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	state := loadJourneyState(t, project)
	if state.Project.SchemaVersion != 5 || state.Lock.Dependencies[0].Kind != dependency.ResolutionLocal || state.Lock.Dependencies[0].Path != filepath.ToSlash(pathFromProject) {
		t.Fatalf("state: %+v", state)
	}
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "check")
	list := project.runBinary(binary, 0, "list")
	if !strings.Contains(list.output(), "local") || !strings.Contains(list.output(), "authorized on this machine") {
		t.Fatalf("list missing local warning: %s", list.output())
	}
	if !reflect.DeepEqual(originalSource, snapshotProjectTree(t, source)) {
		t.Fatal("install/realize mutated source")
	}
	before := project.snapshot()
	project.runBinary(binary, 0, "install")
	project.assertUnchanged(before, "unchanged local refresh")
	if err := os.WriteFile(filepath.Join(source, "rules/boundaries.md"), []byte("# Edited local guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"realize"}, {"realize", "--dry-run"}, {"check"}} {
		result := project.runBinary(binary, 1, args...)
		if !strings.Contains(result.output(), "local_source_changed") {
			t.Fatalf("wrong drift refusal: %s", result.output())
		}
		project.assertUnchanged(before, "locked local drift")
	}
	project.runBinary(binary, 0, "install")
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "check")
	// A fresh clone has only committed state and no transaction claim paths.
	copied := &journeyProject{t: t, root: t.TempDir(), stateHome: t.TempDir()}
	if err := os.Mkdir(filepath.Join(copied.root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents.yaml", ".agents/registry.lock"} {
		data, err := os.ReadFile(filepath.Join(project.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copied.root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copiedBefore := copied.snapshot()
	copied.runBinary(binary, 1, "realize")
	copied.assertUnchanged(copiedBefore, "unauthorized clone before recovery")
	// A second machine has identical project/source bytes but no local grant.
	project.stateHome = t.TempDir()
	t.Setenv("ACR_STATE_HOME", project.stateHome)
	before = project.snapshot()
	for _, args := range [][]string{{"realize"}, {"check"}, {"install"}, {"freshness", "run", "--policy", "install"}} {
		result := project.runBinary(binary, 1, args...)
		if !strings.Contains(result.output(), "local_source_unauthorized") {
			t.Fatalf("implicit adoption: %s", result.output())
		}
		project.assertUnchanged(before, "unauthorized local source")
	}
	project.runBinary(binary, 0, "list")
	project.runBinary(binary, 0, "outdated")
	project.runBinary(binary, 0, "install", "file:"+pathFromProject)
	project.runBinary(binary, 0, "realize")
	// Freshness install refreshes edits only after explicit directory consent.
	if err := os.WriteFile(filepath.Join(source, "rules/boundaries.md"), []byte("# Freshness revision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := newJourneyClock()
	project.useClock(clock)
	if err := (freshness.Store{BaseDirectory: project.stateHome}).Write(project.root, clock.Now().Add(-freshness.Window), freshness.PolicyInstall, freshness.OutcomeFailed); err != nil {
		t.Fatal(err)
	}
	project.run(0, "freshness", "run", "--policy", "install")
	project.runBinary(binary, 0, "check")
	// Uninstall needs neither the source nor the grant.
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	project.stateHome = t.TempDir()
	t.Setenv("ACR_STATE_HOME", project.stateHome)
	project.runBinary(binary, 0, "uninstall", pkg.source)
	state = loadJourneyState(t, project)
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 || len(ledger.Targets) != 0 {
		t.Fatalf("uninstall left local ownership: %+v", state)
	}
	if state.Project.SchemaVersion != 2 || state.Lock.SchemaVersion != 2 {
		t.Fatalf("uninstall did not downgrade schema: %+v", state)
	}
}

func TestLocalMatchesReleaseNativeOutput(t *testing.T) {
	remote := newJourneyGitHub(t)
	pkg := newJourneyPackage(t, "local/parity", "1.0.0")
	remote.SeedRelease(pkg.fullName, pkg.tag, pkg.commit, pkg.archive)
	project := newJourneyProject(t, remote)
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	project.run(0, "init", "--agent", "codex", "--agent", "claude-code", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	project.run(0, "install", "file:"+source)
	project.run(0, "realize")
	local := loadJourneyState(t, project)
	before := project.snapshot()
	project.run(0, "install", pkg.source)
	released := loadJourneyState(t, project)
	if released.Lock.Dependencies[0].ContentHash != local.Lock.Dependencies[0].ContentHash {
		t.Fatal("release/local inventory hashes differ")
	}
	if released.Project.SchemaVersion != 2 || released.Lock.SchemaVersion != 2 || released.Project.Dependencies[0].Path != "" {
		t.Fatalf("remote replacement retained local declaration: %+v", released)
	}
	project.run(0, "realize")
	after := project.snapshot()
	for _, statePath := range []string{"agents.yaml", ".agents/registry.lock"} {
		delete(before, statePath)
		delete(after, statePath)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("local/release native output differs\nbefore=%v\nafter=%v", before, after)
	}
	if !reflect.DeepEqual(local.Lock.Realization, loadJourneyState(t, project).Lock.Realization) {
		t.Fatal("local/release ledger differs")
	}
}

func TestLocalDotInstallUsesSelectedProject(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	pkg := newJourneySmallPackage(t, "local/self", "1.0.0")
	if err := dependency.ExtractPackageArchive(pkg.archive, project.root); err != nil {
		t.Fatal(err)
	}
	project.runBinary(binary, 0, "install", ".", "--agent", "codex", "--freshness", "none", "--non-interactive")
	original := loadJourneyState(t, project).Lock.Dependencies[0]
	if original.Path != "." {
		t.Fatalf("dot path became %q", original.Path)
	}
	project.runBinary(binary, 0, "realize")
	project.runBinary(binary, 0, "install", "./")
	if got := loadJourneyState(t, project).Lock.Dependencies[0]; got.ContentHash != original.ContentHash {
		t.Fatalf("native outputs changed local inventory: %+v", got)
	}
	for _, file := range pkg.files {
		if body, err := os.ReadFile(filepath.Join(project.root, file.path)); err != nil || string(body) != file.body {
			t.Fatalf("source file changed: %s %v", file.path, err)
		}
	}
}

func TestLocalUninstallRevokesAuthorizationWithoutLockRow(t *testing.T) {
	project := newJourneyProject(t, nil)
	pkg := newJourneySmallPackage(t, "local/missing-lock", "1.0.0")
	source := t.TempDir()
	if err := dependency.ExtractPackageArchive(pkg.archive, source); err != nil {
		t.Fatal(err)
	}
	project.run(0, "install", "file:"+source, "--agent", "codex", "--freshness", "none", "--non-interactive")
	state := loadJourneyState(t, project)
	state.Lock.Dependencies = nil
	if err := dependency.WriteState(project.root, state); err != nil {
		t.Fatal(err)
	}
	project.run(0, "uninstall", pkg.source)
	records := 0
	if err := filepath.WalkDir(filepath.Join(project.stateHome, "local"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".json") {
			records++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatal("uninstall retained directory authorization after missing lock")
	}
}
