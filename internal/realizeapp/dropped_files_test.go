package realizeapp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const droppedSkillRoot = "acr__example__first__release"

// droppedFixture realizes a package version that ships a skill with a
// top-level POLICY.md, a nested references directory, and a script, then
// returns a function that publishes the "upgrade" by deleting those files from
// the package and the script from its manifest. The project lock is untouched,
// as it is after `acr update` relocks only the dependency row.
func droppedFixture(t *testing.T) (project string, service *Service, upgrade func()) {
	t.Helper()
	project = t.TempDir()
	packageRoot := t.TempDir()
	writeFixture(t, filepath.Join(packageRoot, "skills", "release", "SKILL.md"), []byte("# release\n"), 0o644)
	writeFixture(t, filepath.Join(packageRoot, "skills", "release", "POLICY.md"), []byte("old policy\n"), 0o644)
	writeFixture(t, filepath.Join(packageRoot, "skills", "release", "references", "ruling.md"), []byte("ruling\n"), 0o644)
	writeFixture(t, filepath.Join(packageRoot, "skills", "release", "references", "kept.md"), []byte("kept\n"), 0o644)
	writeFixture(t, filepath.Join(packageRoot, "scripts", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	full := manifest.Manifest{Artifacts: manifest.Artifacts{
		Skills:  []manifest.SkillArtifact{{ID: "release", Path: "skills/release"}},
		Scripts: []manifest.ScriptArtifact{{ID: "check", Path: "scripts/check.sh"}},
	}}
	loader := &multiPackageLoader{packages: map[string]fixturePackage{firstSource: {root: packageRoot, manifest: full}}, failures: map[string]error{}}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"claude-code", "codex"},
			Dependencies: []dependency.Declaration{{Source: firstSource, Requested: "latest"}},
		},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: firstSource, Requested: "latest", Kind: dependency.ResolutionRelease, ReleaseID: 1,
			Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("1", 64),
		}}},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	initRepository(t, project)
	service = NewService(loader)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("initial realize: %v", err)
	}
	upgrade = func() {
		for _, relative := range []string{"skills/release/POLICY.md", "skills/release/references/ruling.md", "scripts/check.sh"} {
			if err := os.Remove(filepath.Join(packageRoot, filepath.FromSlash(relative))); err != nil {
				t.Fatal(err)
			}
		}
		loader.packages[firstSource] = fixturePackage{root: packageRoot, manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Skills: []manifest.SkillArtifact{{ID: "release", Path: "skills/release"}},
		}}}
	}
	return project, service, upgrade
}

func droppedPaths() []string {
	var paths []string
	for _, agent := range []string{".claude", ".codex"} {
		paths = append(paths,
			agent+"/skills/"+droppedSkillRoot+"/POLICY.md",
			agent+"/skills/"+droppedSkillRoot+"/references/ruling.md",
		)
	}
	return append(paths, ".claude/scripts/acr__example__first__check/check.sh", ".codex/scripts/acr__example__first__check/check.sh")
}

func requireExists(t *testing.T, project string, want bool, paths ...string) {
	t.Helper()
	for _, relative := range paths {
		_, err := os.Lstat(filepath.Join(project, filepath.FromSlash(relative)))
		if want && err != nil {
			t.Fatalf("%s should exist: %v", relative, err)
		}
		if !want && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be gone: %v", relative, err)
		}
	}
}

func droppedLedgerPaths(t *testing.T, project string) map[string]bool {
	t.Helper()
	paths := map[string]bool{}
	for _, target := range loadLedger(t, project).Targets {
		paths[target.Path] = true
	}
	return paths
}

// TestRealizeRemovesManagedFilesAnUpgradeDropped is issue #180: the lock
// records the skill files as realized artifacts, the new package version lacks
// some, the files on disk still match their managedHash, and one realize run
// removes every one of them and updates the ownership ledger.
func TestRealizeRemovesManagedFilesAnUpgradeDropped(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	dropped := droppedPaths()
	requireExists(t, project, true, dropped...)
	for _, relative := range dropped {
		if !droppedLedgerPaths(t, project)[relative] {
			t.Fatalf("lock does not record %s as a realized artifact", relative)
		}
	}
	upgrade()

	result, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err != nil {
		t.Fatalf("realize after upgrade: %v", err)
	}
	removed := map[string]bool{}
	for _, operation := range result.Plan.Operations {
		if operation.Kind == realize.OperationRemove {
			removed[operation.Path] = true
		}
	}
	for _, relative := range dropped {
		if !removed[relative] {
			t.Fatalf("plan does not remove %s: %#v", relative, result.Plan.Operations)
		}
	}
	requireExists(t, project, false, dropped...)
	kept := []string{
		".claude/skills/" + droppedSkillRoot + "/SKILL.md", ".claude/skills/" + droppedSkillRoot + "/references/kept.md",
		".codex/skills/" + droppedSkillRoot + "/SKILL.md", ".codex/skills/" + droppedSkillRoot + "/references/kept.md",
	}
	requireExists(t, project, true, kept...)
	owned := droppedLedgerPaths(t, project)
	for _, relative := range dropped {
		if owned[relative] {
			t.Fatalf("ledger still owns removed %s", relative)
		}
	}
	for _, relative := range kept {
		if !owned[relative] {
			t.Fatalf("ledger lost %s", relative)
		}
	}
	again, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err != nil || again.Plan.HasChanges() && len(again.Plan.Operations) != 0 {
		t.Fatalf("second realize = %#v, %v; want a no-op", again.Plan.Operations, err)
	}
}

// TestRealizeLeavesAnEmptiedDroppedDirectoryRealizable drops every file under
// one subdirectory of a skill and proves the run after it is not refused for
// the directory the removal emptied.
func TestRealizeLeavesAnEmptiedDroppedDirectoryRealizable(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	upgrade()
	packageRoot := service.loader.(*multiPackageLoader).packages[firstSource].root
	if err := os.Remove(filepath.Join(packageRoot, "skills", "release", "references", "kept.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after dropping a whole directory: %v", err)
	}
	requireExists(t, project, false, ".claude/skills/"+droppedSkillRoot+"/references", ".codex/skills/"+droppedSkillRoot+"/references")
	requireExists(t, project, true, ".claude/skills", ".claude")
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after the removal emptied a directory: %v", err)
	}
}

func TestRealizeDryRunShowsDroppedFileRemoval(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	dropped := droppedPaths()
	upgrade()
	before := loadLedger(t, project)

	result, err := service.Run(context.Background(), project, nil, realize.ModeDryRun)
	if err != nil {
		t.Fatalf("dry-run after upgrade: %v", err)
	}
	removed := map[string]bool{}
	for _, operation := range result.Plan.Operations {
		if operation.Kind == realize.OperationRemove {
			removed[operation.Path] = true
		}
	}
	for _, relative := range dropped {
		if !removed[relative] {
			t.Fatalf("dry-run plan does not show removal of %s: %#v", relative, result.Plan.Operations)
		}
	}
	requireExists(t, project, true, dropped...)
	if got := loadLedger(t, project); len(got.Targets) != len(before.Targets) {
		t.Fatalf("dry-run changed the ledger: %d targets, was %d", len(got.Targets), len(before.Targets))
	}
}

func TestRealizeRefusesModifiedDroppedSkillFile(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	modified := ".codex/skills/" + droppedSkillRoot + "/POLICY.md"
	writeFixture(t, filepath.Join(project, modified), []byte("my own edit\n"), 0o644)
	upgrade()
	var recorded string
	for _, target := range loadLedger(t, project).Targets {
		if target.Path == modified {
			recorded = target.Entries[0].ManagedHash
		}
	}
	if recorded == "" {
		t.Fatalf("no recorded managedHash for %s", modified)
	}

	_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil {
		t.Fatal("realize removed or ignored a dropped file whose content was edited")
	}
	for _, want := range []string{modified, recorded, "invalid_skill_tree"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	content, readErr := os.ReadFile(filepath.Join(project, modified))
	if readErr != nil || string(content) != "my own edit\n" {
		t.Fatalf("edited file = %q, %v", content, readErr)
	}
	requireExists(t, project, true, droppedPaths()...)
}

func TestRealizeRefusesModifiedDroppedNonSkillFile(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	modified := ".claude/scripts/acr__example__first__check/check.sh"
	writeFixture(t, filepath.Join(project, modified), []byte("#!/bin/sh\necho edited\n"), 0o755)
	upgrade()
	var recorded string
	for _, target := range loadLedger(t, project).Targets {
		if target.Path == modified {
			recorded = target.Entries[0].ManagedHash
		}
	}

	result, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil {
		t.Fatalf("realize removed an edited dropped script: %#v", result.Plan.Operations)
	}
	var conflict string
	for _, operation := range result.Plan.Operations {
		if operation.Kind == realize.OperationConflict && operation.Path == modified {
			conflict = operation.Reason
		}
	}
	for _, want := range []string{modified, recorded} {
		if !strings.Contains(conflict, want) {
			t.Fatalf("conflict %q does not name %q", conflict, want)
		}
	}
	requireExists(t, project, true, droppedPaths()...)
}

// TestRealizeRollsBackDroppedFileRemovalOnLaterFailure fails the state write
// that finalizes the transaction and proves the removals are undone with it.
func TestRealizeRollsBackDroppedFileRemovalOnLaterFailure(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	dropped := droppedPaths()
	upgrade()
	ledgerBefore, err := os.ReadFile(filepath.Join(project, dependency.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected finalization failure")
	service.marshalState = func(dependency.State) ([]byte, []byte, error) { return nil, nil, injected }

	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); !errors.Is(err, injected) {
		t.Fatalf("realize error = %v, want the injected failure", err)
	}
	requireExists(t, project, true, dropped...)
	for _, relative := range dropped {
		content, readErr := os.ReadFile(filepath.Join(project, filepath.FromSlash(relative)))
		if readErr != nil || len(content) == 0 {
			t.Fatalf("%s after rollback = %q, %v", relative, content, readErr)
		}
	}
	ledgerAfter, err := os.ReadFile(filepath.Join(project, dependency.LockFilename))
	if err != nil || string(ledgerAfter) != string(ledgerBefore) {
		t.Fatalf("lock changed by a rolled-back realize: %v", err)
	}
	service.marshalState = dependency.MarshalState
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after the injected failure cleared: %v", err)
	}
	requireExists(t, project, false, dropped...)
}

// TestDroppedRemovalLeavesOtherPackagesArtifactsAlone proves a path another
// package still renders is never treated as dropped.
func TestDroppedRemovalLeavesOtherPackagesArtifactsAlone(t *testing.T) {
	project, loader, service := sharedSkillFixture(t, []string{"codex"}, false, firstSource, secondSource)
	initRepository(t, project)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	first := loader.packages[firstSource].root
	if err := os.Remove(filepath.Join(first, "skills", "skill0", "references", "notes.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize: %v", err)
	}
	requireExists(t, project, false, ".codex/skills/acr__example__first__skill0/references/notes.md")
	requireExists(t, project, true,
		".codex/skills/acr__example__first__skill0/SKILL.md",
		".codex/skills/acr__example__second__skill1/SKILL.md",
		".codex/skills/acr__example__second__skill1/references/notes.md",
	)
}

func recordedManagedHash(t *testing.T, project, relative string) string {
	t.Helper()
	for _, target := range loadLedger(t, project).Targets {
		if target.Path == relative {
			return target.Entries[0].ManagedHash
		}
	}
	t.Fatalf("no recorded managedHash for %s", relative)
	return ""
}

// TestRealizeRefusesTrackedDroppedFileBeforeAnyLedgerWrite is the Git-tracked
// case: the plan keeps a tracked target on disk and drops its ownership, so
// validation must refuse the unmodified file while the ledger still owns it.
func TestRealizeRefusesTrackedDroppedFileBeforeAnyLedgerWrite(t *testing.T) {
	for _, scenario := range []struct {
		name, path string
		dropSkill  bool
	}{
		{name: "skill file", path: ".codex/skills/" + droppedSkillRoot + "/POLICY.md"},
		{name: "nested skill file", path: ".codex/skills/" + droppedSkillRoot + "/references/ruling.md"},
		{name: "whole skill", path: ".codex/skills/" + droppedSkillRoot + "/POLICY.md", dropSkill: true},
		{name: "claude script", path: ".claude/scripts/acr__example__first__check/check.sh"},
		{name: "codex script", path: ".codex/scripts/acr__example__first__check/check.sh"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tracked := scenario.path
			project, service, upgrade := droppedFixture(t)
			recorded := recordedManagedHash(t, project, tracked)
			if output, err := exec.Command("git", "-C", project, "add", "-f", tracked).CombinedOutput(); err != nil {
				t.Fatalf("git add: %v: %s", err, output)
			}
			upgrade()
			if scenario.dropSkill {
				loader := service.loader.(*multiPackageLoader)
				fixture := loader.packages[firstSource]
				fixture.manifest.Artifacts.Skills = nil
				loader.packages[firstSource] = fixture
			}
			lockBefore, err := os.ReadFile(filepath.Join(project, dependency.LockFilename))
			if err != nil {
				t.Fatal(err)
			}

			for attempt := 0; attempt < 2; attempt++ {
				_, err = service.Run(context.Background(), project, nil, realize.ModeApply)
				if err == nil {
					t.Fatal("realize dropped ownership of a tracked file that it left on disk")
				}
				for _, want := range []string{tracked, recorded, "tracked by Git", "not modified", "git rm"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q does not contain %q", err, want)
					}
				}
				for _, unwanted := range []string{"mismatch", "no longer matches", "unexpected file"} {
					if strings.Contains(err.Error(), unwanted) {
						t.Fatalf("refusal %q must not say %q about an unmodified file", err, unwanted)
					}
				}
				lockAfter, readErr := os.ReadFile(filepath.Join(project, dependency.LockFilename))
				if readErr != nil || string(lockAfter) != string(lockBefore) {
					t.Fatalf("the lock changed before the refusal: %v", readErr)
				}
				if !droppedLedgerPaths(t, project)[tracked] {
					t.Fatal("ledger no longer owns the tracked file")
				}
				requireExists(t, project, true, droppedPaths()...)
			}
			application := &Application{service: service, fallback: cli.UnavailableApplication{}}
			stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", project, "--json")
			if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) || !strings.Contains(stderr, recorded) {
				t.Fatalf("tracked-file CLI refusal: exit %d, stdout %q, stderr %q", exitCode, stdout, stderr)
			}

			// Once the user removes the tracked file, the next realize clears the rest.
			if output, err := exec.Command("git", "-C", project, "rm", "-f", "-q", tracked).CombinedOutput(); err != nil {
				t.Fatalf("git rm: %v: %s", err, output)
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatalf("realize after git rm: %v", err)
			}
			requireExists(t, project, false, droppedPaths()...)
		})
	}
}

// TestRealizeNamesEditedFileWhenItsWholeDirectoryWasDropped drops every file
// under a skill subdirectory while one of them was edited: the refusal names
// that file and its managedHash, not the directory.
func TestRealizeNamesEditedFileWhenItsWholeDirectoryWasDropped(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	edited := ".codex/skills/" + droppedSkillRoot + "/references/ruling.md"
	recorded := recordedManagedHash(t, project, edited)
	writeFixture(t, filepath.Join(project, edited), []byte("my ruling\n"), 0o644)
	upgrade()
	packageRoot := service.loader.(*multiPackageLoader).packages[firstSource].root
	if err := os.Remove(filepath.Join(packageRoot, "skills", "release", "references", "kept.md")); err != nil {
		t.Fatal(err)
	}

	_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil {
		t.Fatal("realize ignored an edited file in a dropped directory")
	}
	for _, want := range []string{edited, recorded, "invalid_skill_tree"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "unexpected directory") {
		t.Fatalf("refusal %q names the directory instead of the edited file", err)
	}
	if content, readErr := os.ReadFile(filepath.Join(project, edited)); readErr != nil || string(content) != "my ruling\n" {
		t.Fatalf("edited file = %q, %v", content, readErr)
	}
	requireExists(t, project, true, droppedPaths()...)
}

// TestRealizeSaysWhenOnlyTheModeOfADroppedFileChanged covers the skill-tree
// refusal and the planner conflict: the content still matches, so the message
// must not say it does not.
func TestRealizeSaysWhenOnlyTheModeOfADroppedFileChanged(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	skillFile := ".codex/skills/" + droppedSkillRoot + "/POLICY.md"
	recorded := recordedManagedHash(t, project, skillFile)
	if err := os.Chmod(filepath.Join(project, skillFile), 0o600); err != nil {
		t.Fatal(err)
	}
	upgrade()
	_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil {
		t.Fatal("realize removed a dropped file whose mode changed")
	}
	for _, want := range []string{skillFile, recorded, "mode changed from 0644 to 0600", "chmod 0644 " + skillFile} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("refusal %q says the content changed", err)
	}
	if err := os.Chmod(filepath.Join(project, skillFile), 0o644); err != nil {
		t.Fatal(err)
	}

	script := ".claude/scripts/acr__example__first__check/check.sh"
	scriptHash := recordedManagedHash(t, project, script)
	if err := os.Chmod(filepath.Join(project, script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil {
		t.Fatal("realize removed a dropped script whose mode changed")
	}
	var conflict string
	for _, operation := range result.Plan.Operations {
		if operation.Kind == realize.OperationConflict && operation.Path == script {
			conflict = operation.Reason
		}
	}
	for _, want := range []string{script, scriptHash, "mode changed from 0755 to 0700", "chmod 0755 " + script} {
		if !strings.Contains(conflict, want) {
			t.Fatalf("conflict %q does not contain %q", conflict, want)
		}
	}
	if strings.Contains(conflict, "no longer matches") {
		t.Fatalf("conflict %q says the content changed", conflict)
	}
}
