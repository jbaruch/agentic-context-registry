package realizeapp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
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
	requireExists(t, project, true, ".claude/skills/"+droppedSkillRoot+"/references", ".codex/skills/"+droppedSkillRoot+"/references")
	requireExists(t, project, true, ".claude/skills", ".claude")
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after the removal emptied a directory: %v", err)
	}
	if result, err := service.Run(context.Background(), project, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
		t.Fatalf("check after directory removal = %#v, %v", result.Plan, err)
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
	for _, want := range []string{modified, recorded, "refusing to remove modified managed output"} {
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
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind the superuser")
	}
	project, service, upgrade := droppedFixture(t)
	dropped := droppedPaths()
	upgrade()
	private := filepath.Dir(filepath.Join(project, ".claude/scripts/acr__example__first__check/check.sh"))
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	// A mode change forces agents.yaml to be written after the native removals.
	if err := os.Chmod(filepath.Join(project, dependency.ProjectFilename), 0o600); err != nil {
		t.Fatal(err)
	}
	type snapshot struct {
		content []byte
		info    os.FileInfo
	}
	before := map[string]snapshot{}
	for _, relative := range append(droppedPaths(), dependency.ProjectFilename, dependency.LockFilename, ".git/info/exclude") {
		filename := filepath.Join(project, filepath.FromSlash(relative))
		content, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		before[relative] = snapshot{content, info}
	}
	// Keep the original inode alive so it cannot be reused after removal.
	// A different inode afterward proves the file was removed and restored.
	original, err := os.Open(filepath.Join(project, dropped[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err := os.Chmod(project, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(project, 0o755); err != nil {
			t.Error(err)
		}
	})

	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), `apply update "agents.yaml"`) || !strings.Contains(err.Error(), "all filesystem changes were rolled back") {
		t.Fatalf("realize error = %v, want a late agents.yaml write failure with rollback", err)
	}
	requireExists(t, project, true, dropped...)
	for relative, snapshot := range before {
		content, readErr := os.ReadFile(filepath.Join(project, filepath.FromSlash(relative)))
		if readErr != nil || string(content) != string(snapshot.content) {
			t.Fatalf("%s after rollback = %q, %v", relative, content, readErr)
		}
		info, err := os.Stat(filepath.Join(project, filepath.FromSlash(relative)))
		if err != nil || info.Mode().Perm() != snapshot.info.Mode().Perm() {
			t.Fatalf("%s mode after rollback = %v, %v", relative, info, err)
		}
		if relative == dropped[0] && os.SameFile(info, snapshot.info) {
			t.Fatal("dropped file was never removed and restored")
		}
	}
	if info, err := os.Stat(private); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory after rollback = %v, %v; want 0700", info, err)
	}
	if err := os.Chmod(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after the injected failure cleared: %v", err)
	}
	requireExists(t, project, false, dropped...)
}

func TestRealizeRefusesTrackedDroppedSharedSkillFile(t *testing.T) {
	project, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, firstSource)
	initRepository(t, project)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	const tracked = ".agents/skills/acr__example__first__skill0/references/notes.md"
	recorded := recordedManagedHash(t, project, tracked)
	if output, err := exec.Command("git", "-C", project, "add", "-f", tracked).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize an unchanged tracked shared skill: %v", err)
	}
	if err := os.Remove(filepath.Join(loader.packages[firstSource].root, "skills", "skill0", "references", "notes.md")); err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.ReadFile(filepath.Join(project, dependency.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	contentBefore, err := os.ReadFile(filepath.Join(project, tracked))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
		var conflict *realize.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("shared tracked file refusal = %v, want conflict", err)
		}
		for _, want := range []string{tracked, recorded, "tracked by Git", "git rm"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not contain %q", err, want)
			}
		}
		if got, err := os.ReadFile(filepath.Join(project, dependency.LockFilename)); err != nil || string(got) != string(lockBefore) {
			t.Fatalf("refusal changed ledger bytes: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(project, tracked)); err != nil || string(got) != string(contentBefore) {
			t.Fatalf("refusal changed tracked bytes: %q, %v", got, err)
		}
		if !droppedLedgerPaths(t, project)[tracked] {
			t.Fatal("refusal discarded coordinator ownership")
		}
	}
	if output, err := exec.Command("git", "-C", project, "rm", "-f", "-q", tracked).CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v: %s", err, output)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after git rm: %v", err)
	}
	if droppedLedgerPaths(t, project)[tracked] {
		t.Fatal("ledger retains removed shared skill file")
	}
	requireExists(t, project, false, tracked)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after successful removal: %v", err)
	}
}

// TestDroppedRemovalLeavesOtherPackagesArtifactsAlone proves a path another
// package still renders is never treated as dropped.
func TestDroppedRemovalLeavesOtherPackagesArtifactsAlone(t *testing.T) {
	project, loader, service := sharedSkillFixture(t, []string{"codex"}, false, firstSource, secondSource)
	initRepository(t, project)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	before := loadLedger(t, project)
	first := loader.packages[firstSource].root
	if err := os.Remove(filepath.Join(first, "skills", "skill0", "references", "notes.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize: %v", err)
	}
	after := loadLedger(t, project)
	var expected []realize.Target
	for _, target := range before.Targets {
		if target.Path != ".codex/skills/acr__example__first__skill0/references/notes.md" {
			expected = append(expected, target)
		}
	}
	if !reflect.DeepEqual(after.Targets, expected) {
		t.Fatal("surviving ledger rows changed")
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
				for _, want := range []string{tracked, recorded, "tracked by Git", "review it", "git rm"} {
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
	for _, want := range []string{edited, recorded, "refusing to remove modified managed output"} {
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

func TestTrackedUninstallReleasesWholeFiles(t *testing.T) {
	for _, scenario := range []struct{ source, skill string }{
		{firstSource, "acr__example__first__skill0"},
		{freshness.Source, "acr__jbaruch__agentic-context-registry__skill0"},
	} {
		t.Run(scenario.source, func(t *testing.T) {
			project, _, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, scenario.source, secondSource)
			initRepository(t, project)
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			tracked := []string{
				".claude/skills/" + scenario.skill + "/references/notes.md",
				".codex/skills/" + scenario.skill + "/references/notes.md",
				".agents/skills/" + scenario.skill + "/references/notes.md",
			}
			args := append([]string{"-C", project, "add", "-f"}, tracked...)
			if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git add: %v: %s", err, output)
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			before := loadLedger(t, project)
			contents := map[string]string{}
			for _, name := range tracked {
				body, err := os.ReadFile(filepath.Join(project, name))
				if err != nil {
					t.Fatal(err)
				}
				contents[name] = string(body)
			}
			if _, err := service.Uninstall(context.Background(), project, scenario.source, false); err != nil {
				t.Fatalf("uninstall tracked files: %v", err)
			}
			after := loadLedger(t, project)
			for _, old := range before.Targets {
				if old.Entries[0].Source == scenario.source && old.Entries[0].ArtifactID != freshness.ArtifactID {
					if droppedLedgerPaths(t, project)[old.Path] {
						t.Fatalf("uninstall kept ownership of %s", old.Path)
					}
					continue
				}
				found := false
				for _, next := range after.Targets {
					if reflect.DeepEqual(next, old) {
						found = true
					}
				}
				if !found {
					t.Fatalf("uninstall changed surviving ledger row %s", old.Path)
				}
			}
			indexed, err := realize.TrackedPaths(project, tracked)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range tracked {
				if body, err := os.ReadFile(filepath.Join(project, name)); err != nil || string(body) != contents[name] || !indexed[name] {
					t.Fatalf("tracked file %s changed: %q, indexed=%t, %v", name, body, indexed[name], err)
				}
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeCheck); err != nil {
				t.Fatalf("post-uninstall check: %v", err)
			}
		})
	}
}

func TestDroppedIndexedFileAlreadyMissing(t *testing.T) {
	for _, prefix := range []string{".codex", ".agents"} {
		t.Run(prefix, func(t *testing.T) {
			project, loader, service := sharedSkillFixture(t, []string{"codex"}, true, firstSource)
			initRepository(t, project)
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			name := prefix + "/skills/acr__example__first__skill0/references/notes.md"
			if output, err := exec.Command("git", "-C", project, "add", "-f", name).CombinedOutput(); err != nil {
				t.Fatalf("git add: %v: %s", err, output)
			}
			for _, filename := range []string{filepath.Join(project, name), filepath.Join(loader.packages[firstSource].root, "skills/skill0/references/notes.md")} {
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatalf("already missing indexed file: %v", err)
			}
			if droppedLedgerPaths(t, project)[name] {
				t.Fatal("ledger retained missing file")
			}
			indexed, err := realize.TrackedPaths(project, []string{name})
			if err != nil || !indexed[name] {
				t.Fatalf("index changed: %v, %v", indexed, err)
			}
		})
	}
}

func TestTrackedFilesOnRetiredSurfacesReleaseOwnership(t *testing.T) {
	for _, prefix := range []string{".claude", ".agents"} {
		t.Run(prefix, func(t *testing.T) {
			project, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, firstSource)
			initRepository(t, project)
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			name := prefix + "/skills/acr__example__first__skill0/references/notes.md"
			if output, err := exec.Command("git", "-C", project, "add", "-f", name).CombinedOutput(); err != nil {
				t.Fatalf("git add: %v: %s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(project, name))
			if err != nil {
				t.Fatal(err)
			}
			state, err := dependency.LoadState(project)
			if err != nil {
				t.Fatal(err)
			}
			if prefix == ".claude" {
				state.Project.Agents = []string{"codex"}
			} else {
				state.Project.SharedSkills = false
			}
			if err := dependency.WriteState(project, state); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(loader.packages[firstSource].root, "skills/skill0/references/notes.md")); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatalf("retire tracked surface: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(project, name)); err != nil || string(got) != string(body) {
				t.Fatalf("retired tracked bytes = %q, %v", got, err)
			}
			if droppedLedgerPaths(t, project)[name] {
				t.Fatal("retired surface retained ownership")
			}
			if _, err := service.Run(context.Background(), project, nil, realize.ModeCheck); err != nil {
				t.Fatalf("retirement check: %v", err)
			}
		})
	}
}

func TestMixedDroppedFileRefusalWritesNothing(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "modified"
		if tracked {
			name = "tracked and modified"
		}
		t.Run(name, func(t *testing.T) {
			project, service, upgrade := droppedFixture(t)
			edited := ".codex/skills/" + droppedSkillRoot + "/POLICY.md"
			recorded := recordedManagedHash(t, project, edited)
			if tracked {
				if output, err := exec.Command("git", "-C", project, "add", "-f", edited).CombinedOutput(); err != nil {
					t.Fatalf("git add: %v: %s", err, output)
				}
			}
			writeFixture(t, filepath.Join(project, edited), []byte("operator changes\n"), 0o644)
			upgrade()
			before := hashProjectTree(t, project)
			_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
			var conflict *realize.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("mixed refusal = %v", err)
			}
			for _, want := range []string{edited, recorded, "review it"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %v lacks %q", err, want)
				}
			}
			if tracked && strings.Contains(err.Error(), "still matches") {
				t.Fatalf("tracked guard claimed content matched: %v", err)
			}
			if after := hashProjectTree(t, project); !reflect.DeepEqual(after, before) {
				t.Fatal("refusal changed project files, ledger, exclusions or index")
			}
		})
	}
}

func TestTrackedFreshnessRetirementWithRealDependency(t *testing.T) {
	project, loader, service := sharedSkillFixture(t, []string{"codex"}, false, freshness.Source)
	initRepository(t, project)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	const hook = ".codex/hooks/acr__jbaruch__agentic-context-registry__freshness-session-start/session-start.sh"
	const realFile = ".codex/skills/acr__jbaruch__agentic-context-registry__skill0/references/notes.md"
	if output, err := exec.Command("git", "-C", project, "add", "-f", hook, realFile).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	state, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Freshness = "none"
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatalf("retire synthetic hook: %v", err)
	}
	requireExists(t, project, true, hook, realFile)
	if droppedLedgerPaths(t, project)[hook] {
		t.Fatal("synthetic hook ownership was not released")
	}
	recorded := recordedManagedHash(t, project, realFile)
	if err := os.Remove(filepath.Join(loader.packages[freshness.Source].root, "skills/skill0/references/notes.md")); err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), project, nil, realize.ModeApply)
	var conflict *realize.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("real dependency dropped tracked file: %v", err)
	}
	for _, want := range []string{realFile, recorded, "git rm"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %v lacks %q", err, want)
		}
	}
}

func TestDroppedFilesRespectAgentSubsetAndExclusionBytes(t *testing.T) {
	project, service, upgrade := droppedFixture(t)
	beforeLedger := adapterTargets(loadLedger(t, project), "claude-code")
	before := claudeCodeTargets(t, hashProjectTree(t, project))
	exclude := filepath.Join(project, gitExcludeFile)
	body, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	const tail = "# user exclusion after ACR block\n*.operator"
	if err := os.WriteFile(exclude, append(body, []byte(tail)...), 0o644); err != nil {
		t.Fatal(err)
	}
	upgrade()
	if _, err := service.Run(context.Background(), project, []string{"codex"}, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	if got := adapterTargets(loadLedger(t, project), "claude-code"); !reflect.DeepEqual(got, beforeLedger) {
		t.Fatal("subset changed excluded agent ledger rows")
	}
	if got := claudeCodeTargets(t, hashProjectTree(t, project)); !reflect.DeepEqual(got, before) {
		t.Fatal("subset changed excluded agent outputs")
	}
	after, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, ok := strings.Cut(string(body), excludeBeginMarker)
	if !ok || !strings.HasPrefix(string(after), prefix) || !strings.HasSuffix(string(after), tail) {
		t.Fatalf("unmanaged exclusion bytes changed: %q", after)
	}
	for _, name := range droppedPaths() {
		if strings.HasPrefix(name, ".codex/") {
			requireExists(t, project, false, name)
			if strings.Contains(string(after), "/"+name+"\n") {
				t.Fatalf("exclusion retained removed %s", name)
			}
		} else {
			if !strings.Contains(string(after), "/"+name+"\n") {
				t.Fatalf("exclusion lost retained %s", name)
			}
		}
	}
}
