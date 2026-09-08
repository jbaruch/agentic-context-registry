package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// TestJourneyMigrationRetainsUnrelatedACRDependency is the issue #107
// acceptance run through the shipped composition: a project that already
// installed and realized an ACR package migrates its Tessl installation beside
// it. The reported build refused the whole run with project_state_conflict,
// because the unrelated package was not part of the Tessl mapping.
//
// The journey walks the reported sequence — install, realize, check, rehearse,
// apply, repeat, finalize — and holds the unrelated package's declaration,
// lock, realized bytes and file modes still through all of it.
func TestJourneyMigrationRetainsUnrelatedACRDependency(t *testing.T) {
	github := newJourneyGitHub(t)
	alpha := newJourneySmallPackage(t, "example/alpha", "1.0.0")
	unrelated := newJourneySmallPackage(t, "other/unrelated", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)
	github.SeedRelease(unrelated.fullName, unrelated.tag, unrelated.commit, unrelated.archive)

	project := newJourneyProject(t, github)
	project.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	project.run(0, "install", unrelated.source+"@"+unrelated.tag, "--non-interactive")
	project.run(0, "realize")
	project.run(0, "check")

	installed := loadJourneyState(t, project)
	declaredBefore := journeyDeclaration(t, installed, unrelated.source)
	lockedBefore := journeyLock(t, installed, unrelated.source)
	if lockedBefore.Commit != unrelated.commit || lockedBefore.Requested != unrelated.tag {
		t.Fatalf("the unrelated package locked %#v, want the pinned %s", lockedBefore, unrelated.commit)
	}
	hookPath := nativeHookExecutable(".codex", unrelated.fullName, "session-start", "session-start.sh")
	hookBody := unrelated.body(t, "hooks/session-start.sh")
	ruleBody := unrelated.body(t, "rules/sibling.md")
	hostMode := journeyFileMode(t, project, "AGENTS.md")

	// The Tessl installation arrives next to the ACR one, exactly as a
	// gradual migration finds it.
	journeyTesslConsumer(t, project, alpha)
	before := project.snapshot()

	mapping := []string{"migrate", "tessl", "--map", "example/alpha=" + alpha.source + "@" + alpha.tag, "--vendor-unmapped", "--non-interactive"}
	dry := project.run(0, append(append([]string(nil), mapping...), "--dry-run", "--json")...)
	if report := journeyResult(t, dry.stdout); report["wrote"] != false {
		t.Fatalf("acr migrate tessl --dry-run = %#v, want a rehearsal", report)
	}
	project.assertUnchanged(before, "the migration rehearsal")

	project.run(0, mapping...)
	migrated := loadJourneyState(t, project)
	if got := journeyDeclaration(t, migrated, unrelated.source); !reflect.DeepEqual(got, declaredBefore) {
		t.Fatalf("declaration for %s = %#v, want %#v", unrelated.source, got, declaredBefore)
	}
	if got := journeyLock(t, migrated, unrelated.source); !reflect.DeepEqual(got, lockedBefore) {
		t.Fatalf("lock for %s = %#v, want %#v", unrelated.source, got, lockedBefore)
	}
	if len(migrated.Lock.Dependencies) != 3 {
		t.Fatalf("locks = %#v, want the retained, the mapped and the vendored package", migrated.Lock.Dependencies)
	}
	assertProjectFile(t, project, hookPath, hookBody, 0o755)
	assertRetainedRule(t, project, "the migration", ruleBody, hostMode)
	project.run(0, "check")

	settled := project.snapshot()
	project.run(0, mapping...)
	project.assertUnchanged(settled, "a repeated migration")

	// Finalization removes Tessl and nothing else: the unrelated package keeps
	// its state entries, its realized bytes and its executable mode.
	verify8GitCommit(t, project.root)
	finalize := append(append([]string(nil), mapping...), "--finalize")
	project.run(0, finalize...)
	assertProjectAbsent(t, project, "tessl.json")
	finalized := loadJourneyState(t, project)
	if got := journeyDeclaration(t, finalized, unrelated.source); !reflect.DeepEqual(got, declaredBefore) {
		t.Fatalf("finalization changed the declaration for %s = %#v, want %#v", unrelated.source, got, declaredBefore)
	}
	if got := journeyLock(t, finalized, unrelated.source); !reflect.DeepEqual(got, lockedBefore) {
		t.Fatalf("finalization changed the lock for %s = %#v, want %#v", unrelated.source, got, lockedBefore)
	}
	assertProjectFile(t, project, hookPath, hookBody, 0o755)
	assertRetainedRule(t, project, "finalization", ruleBody, hostMode)
	project.run(0, "check")
}

// assertRetainedRule demands that the unrelated package's rule still occupies
// the shared Markdown host, whose mode is unchanged. The host itself
// legitimately grows the migrated package's own block, so the assertion is on
// the retained package's bytes rather than on the whole file.
func assertRetainedRule(t *testing.T, project *journeyProject, what, body string, mode os.FileMode) {
	t.Helper()
	const path = "AGENTS.md"
	host, err := os.ReadFile(project.path(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(host), body) {
		t.Fatalf("%s dropped the retained rule %q from %s:\n%s", what, body, path, host)
	}
	if got := journeyFileMode(t, project, path); got != mode {
		t.Fatalf("%s changed the mode of %s to %v, want %v", what, path, got, mode)
	}
}

func journeyFileMode(t *testing.T, project *journeyProject, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(project.path(path))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func journeyDeclaration(t *testing.T, state dependency.State, source string) dependency.Declaration {
	t.Helper()
	for _, declaration := range state.Project.Dependencies {
		if declaration.Source == source {
			return declaration
		}
	}
	t.Fatalf("no declaration for %s in %#v", source, state.Project.Dependencies)
	return dependency.Declaration{}
}

func journeyLock(t *testing.T, state dependency.State, source string) dependency.LockedDependency {
	t.Helper()
	for _, locked := range state.Lock.Dependencies {
		if locked.Source == source {
			return locked
		}
	}
	t.Fatalf("no lock for %s in %#v", source, state.Lock.Dependencies)
	return dependency.LockedDependency{}
}
