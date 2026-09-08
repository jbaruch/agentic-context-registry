package realizeapp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// sharedSkillFixture writes a project whose packages each contribute one skill
// with an executable companion and a nested references tree, so mode and
// companion preservation are observable in every realized copy.
func sharedSkillFixture(t *testing.T, agents []string, sharedSkills bool, sources ...string) (string, *multiPackageLoader, *Service) {
	t.Helper()
	projectRoot := t.TempDir()
	loader := &multiPackageLoader{packages: map[string]fixturePackage{}, failures: map[string]error{}}
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: agents, SharedSkills: sharedSkills},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	for index, source := range sources {
		packageRoot := t.TempDir()
		name := "skill" + strconv.Itoa(index)
		writeFixture(t, filepath.Join(packageRoot, "skills", name, "SKILL.md"), []byte("# "+name+"\n\nSee skills/"+name+"/references/notes.md\n"), 0o644)
		writeFixture(t, filepath.Join(packageRoot, "skills", name, "references", "notes.md"), []byte("notes for "+name+"\n"), 0o644)
		writeFixture(t, filepath.Join(packageRoot, "skills", name, "run.sh"), []byte("#!/bin/sh\necho "+name+"\n"), 0o755)
		loader.packages[source] = fixturePackage{root: packageRoot, manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Skills: []manifest.SkillArtifact{{ID: name, Path: "skills/" + name}},
		}}}
		state.Project.Dependencies = append(state.Project.Dependencies, dependency.Declaration{Source: source, Requested: "latest"})
		state.Lock.Dependencies = append(state.Lock.Dependencies, dependency.LockedDependency{
			Source: source, Requested: "latest", Kind: dependency.ResolutionRelease, ReleaseID: int64(index + 1),
			Tag: "v1." + strconv.Itoa(index) + ".0", Commit: strings.Repeat(string(rune('a'+index)), 40),
			PackageVersion: "1." + strconv.Itoa(index) + ".0", ContentHash: "sha256:" + strings.Repeat(strconv.Itoa(index), 64),
		})
	}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	return projectRoot, loader, NewService(loader)
}

func loadLedger(t *testing.T, projectRoot string) realize.Ledger {
	t.Helper()
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// TestSharedSurfaceMaterializesCompleteSkillTrees is D10: every companion and
// every mode survives into the shared copy exactly as into a per-agent tree.
func TestSharedSurfaceMaterializesCompleteSkillTrees(t *testing.T) {
	projectRoot, _, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, firstSource)
	initRepository(t, projectRoot)
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		".agents/skills/acr__example__first__skill0",
		".claude/skills/acr__example__first__skill0",
		".codex/skills/acr__example__first__skill0",
	}
	for _, root := range roots {
		for relative, wantExecutable := range map[string]bool{
			"SKILL.md":            false,
			"references/notes.md": false,
			"run.sh":              true,
		} {
			filename := filepath.Join(projectRoot, filepath.FromSlash(root), filepath.FromSlash(relative))
			info, err := os.Lstat(filename)
			if err != nil {
				t.Fatalf("%s/%s: %v", root, relative, err)
			}
			if executable := info.Mode().Perm()&0o111 != 0; executable != wantExecutable {
				t.Fatalf("%s/%s mode = %v, want executable = %v", root, relative, info.Mode().Perm(), wantExecutable)
			}
		}
		content, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(root), "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), root+"/references/notes.md") {
			t.Fatalf("%s/SKILL.md references were not rebased:\n%s", root, content)
		}
	}

	ledger := loadLedger(t, projectRoot)
	if ledger.SchemaVersion != realize.SharedSurfaceLedgerSchemaVersion {
		t.Fatalf("ledger schemaVersion = %d, want %d", ledger.SchemaVersion, realize.SharedSurfaceLedgerSchemaVersion)
	}
	owned := 0
	for _, target := range ledger.Targets {
		if !strings.HasPrefix(target.Path, ".agents/skills/") {
			continue
		}
		owned++
		if target.Owner != realize.OwnerCoordinator {
			t.Fatalf("shared target %q owner = %q", target.Path, target.Owner)
		}
	}
	if owned != 3 {
		t.Fatalf("shared targets recorded = %d, want 3", owned)
	}
}

// TestSharedSurfaceRefusesToOverwriteUserContent is D9.
func TestSharedSurfaceRefusesToOverwriteUserContent(t *testing.T) {
	projectRoot, _, service := sharedSkillFixture(t, []string{"claude-code"}, true, firstSource)
	initRepository(t, projectRoot)
	writeFixture(t, filepath.Join(projectRoot, ".agents", "skills", "acr__example__first__skill0", "SKILL.md"), []byte("# Mine\n"), 0o644)

	result, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply)
	if err == nil {
		t.Fatalf("realize succeeded over user content: %#v", result.Plan.Operations)
	}
	content, readErr := os.ReadFile(filepath.Join(projectRoot, ".agents", "skills", "acr__example__first__skill0", "SKILL.md"))
	if readErr != nil || string(content) != "# Mine\n" {
		t.Fatalf("user file = %q, %v", content, readErr)
	}
}

// TestAgentSubsetLeavesTheSharedSurfaceAlone is D27: a temporary --agent
// subset neither re-derives the surface nor fails closed on it.
func TestAgentSubsetLeavesTheSharedSurfaceAlone(t *testing.T) {
	projectRoot, _, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, firstSource)
	initRepository(t, projectRoot)
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	before := hashProjectTree(t, projectRoot)

	result, err := service.Run(context.Background(), projectRoot, []string{"claude-code"}, realize.ModeApply)
	if err != nil {
		t.Fatalf("--agent claude-code: %v", err)
	}
	if result.Plan.HasChanges() {
		t.Fatalf("subset run planned changes: %#v", result.Plan.Operations)
	}
	after := hashProjectTree(t, projectRoot)
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("subset run changed %s", path)
		}
	}
	shared := 0
	for _, target := range loadLedger(t, projectRoot).Targets {
		if target.Owner == realize.OwnerCoordinator {
			shared++
		}
	}
	if shared != 3 {
		t.Fatalf("shared targets after subset run = %d, want 3", shared)
	}
}

// TestPersistedSelectionRederivesTheSharedSurface is D28: dropping an agent
// from agents.yaml re-derives the surface rather than orphaning it, and
// clearing the declaration retires the surface entirely.
func TestPersistedSelectionRederivesTheSharedSurface(t *testing.T) {
	projectRoot, _, service := sharedSkillFixture(t, []string{"claude-code", "codex"}, true, firstSource)
	initRepository(t, projectRoot)
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}

	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Agents = []string{"claude-code"}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after narrowing agents: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".agents/skills/acr__example__first__skill0/SKILL.md")); err != nil {
		t.Fatalf("shared surface lost after narrowing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".codex/skills/acr__example__first__skill0/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("de-selected agent tree survived: %v", err)
	}

	state, err = dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.SharedSkills = false
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatalf("realize after clearing sharedSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".agents/skills/acr__example__first__skill0/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("shared surface survived a cleared declaration: %v", err)
	}
	for _, target := range loadLedger(t, projectRoot).Targets {
		if target.Owner == realize.OwnerCoordinator {
			t.Fatalf("coordinator target %q survived a cleared declaration", target.Path)
		}
	}
	if version := loadLedger(t, projectRoot).SchemaVersion; version != realize.BaselineLedgerSchemaVersion {
		t.Fatalf("ledger schemaVersion = %d after the surface was retired, want %d", version, realize.BaselineLedgerSchemaVersion)
	}
}

// TestUninstallRemovesSharedEntries is D29: uninstall reaches the shared
// surface through the ordinary realization pass, with no unsupported-adapter
// error from the ledger's coordinator entries.
func TestUninstallRemovesSharedEntries(t *testing.T) {
	projectRoot, _, service := sharedSkillFixture(t, []string{"claude-code"}, true, firstSource, secondSource)
	initRepository(t, projectRoot)
	if _, err := service.Run(context.Background(), projectRoot, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}

	result, err := service.Uninstall(context.Background(), projectRoot, secondSource, false)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !result.Changed {
		t.Fatal("uninstall reported no change")
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".agents/skills/acr__example__second__skill1/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("uninstalled package kept its shared entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".agents/skills/acr__example__first__skill0/SKILL.md")); err != nil {
		t.Fatalf("surviving package lost its shared entry: %v", err)
	}
}
