package realizeapp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
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

func TestRealizedPythonReplacementsInvalidateRetainedBytecode(t *testing.T) {
	projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, firstSource)
	initRepository(t, projectRoot)
	helper := filepath.Join(loader.packages[firstSource].root, "skills", "skill0", "lib", "prune_result.py")
	writeFixture(t, helper, []byte("VALUE = 'A'\n"), 0o644)
	run := func(mode realize.Mode) Result {
		t.Helper()
		result, err := service.Run(context.Background(), projectRoot, nil, mode)
		if err != nil {
			t.Fatalf("realize %s: %v", mode, err)
		}
		return result
	}
	run(realize.ModeApply)
	roots := skillRoots("acr__example__first__skill0")
	mtimes := make(map[string]time.Time)
	filename := func(root string) string {
		return filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "prune_result.py")
	}
	assertImport := func(root, value string) {
		t.Helper()
		// -B reads the original cache without replacing it.
		command := exec.Command("python3", "-I", "-B", "-c", "import sys; sys.path.insert(0, sys.argv[1]); import prune_result; print(prune_result.VALUE)", filepath.Dir(filename(root)))
		if output, err := command.CombinedOutput(); err != nil || string(output) != value+"\n" {
			t.Fatalf("fresh import in %s = %q, %v; want %s", root, output, err, value)
		}
	}
	// Put all original sources and timestamp caches in the same fixed second.
	second := time.Unix(946684800, 0)
	for _, root := range roots {
		if err := os.Chtimes(filename(root), second, second); err != nil {
			t.Fatal(err)
		}
		mtimes[root] = second
	}
	assertCaches := importSkillHelperCaches(t, projectRoot, roots)
	// Demonstrate the exact equal-length/same-second collision with a real
	// interpreter before exercising ACR's replacements. Restore A before
	// realization so ownership checks see the original managed source.
	for _, root := range roots {
		writeFixture(t, filename(root), []byte("VALUE = 'B'\n"), 0o644)
		if err := os.Chtimes(filename(root), second, second); err != nil {
			t.Fatal(err)
		}
		assertImport(root, "A")
		writeFixture(t, filename(root), []byte("VALUE = 'A'\n"), 0o644)
		if err := os.Chtimes(filename(root), second, second); err != nil {
			t.Fatal(err)
		}
	}
	assertCurrent := func() {
		t.Helper()
		if result := run(realize.ModeApply); result.Plan.HasChanges() {
			t.Fatal("no-op realization reported changes")
		}
		run(realize.ModeDryRun)
		run(realize.ModeCheck)
		for _, root := range roots {
			info, err := os.Stat(filename(root))
			if err != nil || !info.ModTime().Equal(mtimes[root]) {
				t.Fatalf("no-op/dry-run/check changed %s timestamp: %v, %v", root, info, err)
			}
		}
		assertCaches()
	}
	assertCurrent()
	for _, generation := range []struct {
		value string
		mode  os.FileMode
	}{
		{"B", 0o644},
		{"C", 0o644},
		{"C", 0o755}, // A mode-only replacement must keep advancing too.
	} {
		writeFixture(t, helper, []byte("VALUE = '"+generation.value+"'\n"), generation.mode)
		// Package generations deliberately have equal lengths and identical
		// whole-second mtimes; source metadata is no freshness guarantee.
		if err := os.Chtimes(helper, second, second); err != nil {
			t.Fatal(err)
		}
		run(realize.ModeDryRun)
		assertCaches()
		run(realize.ModeApply)
		for _, root := range roots {
			info, err := os.Stat(filename(root))
			if err != nil {
				t.Fatal(err)
			}
			if info.ModTime().Unix() <= mtimes[root].Unix() || info.ModTime().Nanosecond() != 0 || info.Mode().Perm() != generation.mode {
				t.Fatalf("%s replacement metadata = %s, %04o; previous timestamp %s", root, info.ModTime(), info.Mode().Perm(), mtimes[root])
			}
			mtimes[root] = info.ModTime()
			assertImport(root, generation.value)
		}
		assertCurrent()
	}
}

// TestRealizedSkillBytecodeCacheDoesNotBlockRealize is issue #183: importing
// a realized helper creates runtime output that remains outside ACR ownership.
func TestRealizedSkillBytecodeCacheDoesNotBlockRealize(t *testing.T) {
	policy := "github:example/policy"
	other := "github:example/other"
	projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, policy, other)
	initRepository(t, projectRoot)
	packageRoot := loader.packages[policy].root
	writeFixture(t, filepath.Join(packageRoot, "skills", "skill0", "lib", "prune_result.py"), []byte("def prune():\n    return None\n"), 0o644)
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}
	run := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := runCLI(t, application, append(args, "--project", projectRoot)...)
		if code != cli.ExitSuccess || stderr != "" {
			t.Fatalf("%v = %d, %q, %q", args, code, stdout, stderr)
		}
		return stdout
	}
	current := func() {
		t.Helper()
		if stdout := run("realize"); !strings.Contains(stdout, "Realization is already current for ") {
			t.Fatalf("realize = %q, want current", stdout)
		}
		if stdout := run("check"); !strings.Contains(stdout, "Realization is current for ") {
			t.Fatalf("check = %q, want current", stdout)
		}
	}
	run("realize")
	roots := skillRoots("acr__example__policy__skill0")
	otherRoots := skillRoots("acr__example__other__skill1")
	assertCaches := importSkillHelperCaches(t, projectRoot, roots)
	for _, root := range otherRoots {
		plantRootBytecodeCache(t, projectRoot, root)
	}
	current()
	assertCaches()
	if stdout := run("realize", "--dry-run"); !strings.Contains(stdout, "Realization would apply 0 change(s) for ") {
		t.Fatalf("dry-run = %q", stdout)
	}
	assertCaches()
	beforeSubset := hashProjectTree(t, projectRoot)
	run("realize", "--agent", "codex")
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(after, beforeSubset) {
		t.Fatal("selected-adapter realize changed an unselected surface")
	}
	assertRootBytecodeCaches(t, projectRoot, otherRoots, true)

	updatedHelper := []byte("def prune():\n    return 42\n")
	writeFixture(t, filepath.Join(packageRoot, "skills", "skill0", "lib", "prune_result.py"), updatedHelper, 0o644)
	run("realize")
	for _, root := range roots {
		content, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "prune_result.py"))
		if err != nil || !bytes.Equal(content, updatedHelper) {
			t.Fatalf("updated helper %s = %q, %v", root, content, err)
		}
	}
	assertCaches()
	for _, root := range roots[:3] {
		for _, relative := range []string{"lib/__pycache__/notes.txt", "extra.md"} {
			filename := filepath.Join(projectRoot, filepath.FromSlash(root+"/"+relative))
			writeFixture(t, filename, []byte("unexpected\n"), 0o644)
			before := hashProjectTree(t, projectRoot)
			for _, command := range []string{"realize", "check"} {
				stdout, stderr, code := runCLI(t, application, command, "--project", projectRoot)
				if code != cli.ExitConflict || !strings.Contains(stderr, "invalid_skill_tree") {
					t.Fatalf("%s unexpected %s = %d, %q, %q", command, relative, code, stdout, stderr)
				}
			}
			if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(after, before) {
				t.Fatal("native validation refusal changed the project")
			}
			if err := os.Remove(filename); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Shared skill content keeps its baseline compatibility: no new tree walk.
	sharedExtra := filepath.Join(projectRoot, filepath.FromSlash(roots[3]), "extra.md")
	writeFixture(t, sharedExtra, []byte("shared user content\n"), 0o640)
	current()
	if got := string(readFile(t, sharedExtra)); got != "shared user content\n" {
		t.Fatalf("shared undeclared content = %q", got)
	}
	assertCaches()

	pkg := loader.packages[policy]
	pkg.manifest.Artifacts.Skills[0].ID = "release"
	loader.packages[policy] = pkg
	run("realize")
	for _, root := range roots {
		assertRetiredSkill(t, projectRoot, root)
	}
	assertCaches()
	current()
	assertRootBytecodeCaches(t, projectRoot, otherRoots, true)
	releaseRoots := skillRoots("acr__example__policy__release")
	assertReleaseCaches := importSkillHelperCaches(t, projectRoot, releaseRoots)
	run("uninstall", policy, "--dry-run")
	assertReleaseCaches()
	for _, root := range releaseRoots {
		if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root), "SKILL.md")); err != nil {
			t.Fatalf("dry-run removed managed skill %s: %v", root, err)
		}
	}
	run("uninstall", policy)
	for _, root := range releaseRoots {
		assertRetiredSkill(t, projectRoot, root)
	}
	assertCaches()
	assertReleaseCaches()
	current()
	run("uninstall", other)
	for _, root := range otherRoots {
		assertRetiredSkill(t, projectRoot, root)
	}
	assertRootBytecodeCaches(t, projectRoot, otherRoots, true)
	assertCaches()
	assertReleaseCaches()
	current()
	if got := string(readFile(t, sharedExtra)); got != "shared user content\n" {
		t.Fatalf("uninstall changed shared undeclared content = %q", got)
	}
}

// importSkillHelperCaches verifies real interpreter output without depending
// on a Python implementation or version-specific bytecode filename.
func importSkillHelperCaches(t *testing.T, projectRoot string, roots []string) func() {
	t.Helper()
	before := make(map[string]map[string]string)
	for _, root := range roots {
		lib := filepath.Join(projectRoot, filepath.FromSlash(root), "lib")
		command := exec.Command("python3", "-I", "-c", "import sys; sys.dont_write_bytecode = False; sys.path.insert(0, sys.argv[1]); import prune_result", lib)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("import realized Python helper in %s: %v: %s", root, err, output)
		}
		cache := filepath.Join(lib, "__pycache__")
		entries, err := os.ReadDir(cache)
		if err != nil || len(entries) == 0 {
			t.Fatalf("Python did not create bytecode in %s: %v", cache, err)
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(entry.Name(), ".pyc") {
				t.Fatalf("unexpected Python cache entry %s: %v, %v", entry.Name(), info, err)
			}
		}
		before[cache] = hashProjectTree(t, cache)
	}
	return func() {
		t.Helper()
		for cache, want := range before {
			if got := hashProjectTree(t, cache); !reflect.DeepEqual(got, want) {
				t.Errorf("runtime cache bytes/modes changed in %s: got %v, want %v", cache, got, want)
			}
		}
		for _, target := range loadLedger(t, projectRoot).Targets {
			if strings.Contains(target.Path, "/__pycache__/") {
				t.Errorf("runtime cache entered ownership ledger: %s", target.Path)
			}
		}
	}
}

func assertRetiredSkill(t *testing.T, projectRoot, root string) {
	t.Helper()
	for _, target := range loadLedger(t, projectRoot).Targets {
		if strings.HasPrefix(target.Path, root+"/") {
			t.Errorf("retired ownership remains for %s", target.Path)
		}
	}
	for _, relative := range []string{"SKILL.md", "run.sh", "references/notes.md", "lib/prune_result.py"} {
		if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root+"/"+relative))); !os.IsNotExist(err) {
			t.Errorf("managed file %s/%s = %v, want absent", root, relative, err)
		}
	}
}

// Shared helper retirement keeps baseline behavior across an agent switch.
func TestSharedHelperRetirementPreservesRuntimeCaches(t *testing.T) {
	for _, caches := range []bool{true, false} {
		t.Run("caches="+strconv.FormatBool(caches), func(t *testing.T) {
			projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code"}, true, firstSource)
			initRepository(t, projectRoot)
			helper := filepath.Join(loader.packages[firstSource].root, "skills", "skill0", "lib", "prune_result.py")
			writeFixture(t, helper, []byte("value = 42\n"), 0o644)
			application := &Application{service: service, fallback: cli.UnavailableApplication{}}
			stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
			}
			const root = ".agents/skills/acr__example__first__skill0"
			var assertCaches func()
			if caches {
				assertCaches = importSkillHelperCaches(t, projectRoot, []string{root})
			}
			if err := os.Remove(helper); err != nil {
				t.Fatal(err)
			}
			state, err := dependency.LoadState(projectRoot)
			if err != nil {
				t.Fatal(err)
			}
			state.Project.Agents = []string{"codex"}
			if err := dependency.WriteState(projectRoot, state); err != nil {
				t.Fatal(err)
			}
			before := hashProjectTree(t, projectRoot)
			stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot, "--dry-run")
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("dry-run = %d, %q, %q", code, stdout, stderr)
			}
			stdout, stderr, code = runCLI(t, application, "check", "--project", projectRoot)
			if code != cli.ExitChanges || !strings.Contains(stderr, "realization has") {
				t.Fatalf("check = %d, %q, %q", code, stdout, stderr)
			}
			if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
				t.Fatal("dry-run/check changed the project")
			}
			realizedHelper := filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "prune_result.py")
			for _, changed := range []struct {
				name    string
				content string
				mode    os.FileMode
				want    string
			}{
				{"content", "value = 43\n", 0o644, "refusing to remove modified managed output"},
				{"mode", "value = 42\n", 0o755, "refusing to remove managed output with a changed mode"},
			} {
				writeFixture(t, realizedHelper, []byte(changed.content), changed.mode)
				before := hashProjectTree(t, projectRoot)
				stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot)
				if code != cli.ExitConflict || !strings.Contains(stderr, changed.want) {
					t.Fatalf("modified helper %s = %d, %q, %q", changed.name, code, stdout, stderr)
				}
				if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed project for helper %s", changed.name)
				}
			}
			writeFixture(t, realizedHelper, []byte("value = 42\n"), 0o644)
			stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("retire helper = %d, %q, %q", code, stdout, stderr)
			}
			if _, err := os.Lstat(realizedHelper); !os.IsNotExist(err) {
				t.Fatalf("retired helper = %v, want absent", err)
			}
			for _, target := range loadLedger(t, projectRoot).Targets {
				if target.Path == root+"/lib/prune_result.py" {
					t.Errorf("retired helper ownership remains: %s", target.Path)
				}
			}
			if caches {
				assertCaches()
			}
			for _, command := range []string{"realize", "check", "check"} {
				stdout, stderr, code = runCLI(t, application, command, "--project", projectRoot)
				if code != cli.ExitSuccess || stderr != "" {
					t.Fatalf("%s after helper retirement = %d, %q, %q", command, code, stdout, stderr)
				}
			}
			if caches {
				assertCaches()
			}
		})
	}
}

func TestUninstallPreservesBytecodeAfterManagedSkillFilesAreMissing(t *testing.T) {
	projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, firstSource, secondSource)
	initRepository(t, projectRoot)
	writeFixture(t, filepath.Join(loader.packages[firstSource].root, "skills", "skill0", "lib", "prune_result.py"), []byte("value = 42\n"), 0o644)
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}
	stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
	}
	roots := skillRoots("acr__example__first__skill0")
	otherRoots := skillRoots("acr__example__second__skill1")
	assertCaches := importSkillHelperCaches(t, projectRoot, roots)
	for _, root := range otherRoots {
		plantRootBytecodeCache(t, projectRoot, root)
	}
	// Keep only the generated caches and parent directories of retired files.
	for _, target := range loadLedger(t, projectRoot).Targets {
		for _, root := range roots {
			if strings.HasPrefix(target.Path, root+"/") {
				if err := os.Remove(filepath.Join(projectRoot, filepath.FromSlash(target.Path))); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// No cache or parent removal is authorized, even when the retired root
	// cannot be written after all managed files have already gone missing.
	for _, root := range roots {
		filename := filepath.Join(projectRoot, filepath.FromSlash(root))
		if err := os.Chmod(filename, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(filename, 0o755); err != nil {
				t.Errorf("restore retired skill permissions: %v", err)
			}
		})
	}
	before := hashProjectTree(t, projectRoot)
	stdout, stderr, code = runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--dry-run")
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("dry-run uninstall = %d, %q, %q", code, stdout, stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("dry-run uninstall changed the project")
	}
	assertCaches()
	stdout, stderr, code = runCLI(t, application, "uninstall", firstSource, "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall = %d, %q, %q", code, stdout, stderr)
	}
	for _, root := range roots {
		assertRetiredSkill(t, projectRoot, root)
		if info, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root))); err != nil || !info.IsDir() || info.Mode().Perm() != 0o555 {
			t.Errorf("retired skill root %s: %v, %v; want preserved read-only directory", root, info, err)
		}
		for _, target := range loadLedger(t, projectRoot).Targets {
			if strings.HasPrefix(target.Path, root+"/") {
				t.Errorf("uninstall retained ownership of %s", target.Path)
			}
		}
	}
	assertCaches()
	assertRootBytecodeCaches(t, projectRoot, otherRoots, true)
	stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Realization is already current for ") {
		t.Fatalf("realize after uninstall = %d, %q, %q", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, application, "check", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("check after uninstall = %d, %q, %q", code, stdout, stderr)
	}
	// Retire the remaining package after its files have also gone missing;
	// caches and their containing directories remain after the last removal.
	for _, target := range loadLedger(t, projectRoot).Targets {
		for _, root := range otherRoots {
			if strings.HasPrefix(target.Path, root+"/") {
				if err := os.Remove(filepath.Join(projectRoot, filepath.FromSlash(target.Path))); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	stdout, stderr, code = runCLI(t, application, "uninstall", secondSource, "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("last uninstall = %d, %q, %q", code, stdout, stderr)
	}
	assertCaches()
	assertRootBytecodeCaches(t, projectRoot, otherRoots, true)
	for _, root := range otherRoots {
		assertRetiredSkill(t, projectRoot, root)
	}
	for _, command := range []string{"realize", "check"} {
		stdout, stderr, code = runCLI(t, application, command, "--project", projectRoot)
		if code != cli.ExitSuccess || stderr != "" {
			t.Fatalf("%s after last uninstall = %d, %q, %q", command, code, stdout, stderr)
		}
	}
}

func skillRoots(name string) []string {
	return []string{
		".claude/skills/" + name,
		".codex/skills/" + name,
		".cursor/skills/" + name,
		".agents/skills/" + name,
	}
}

func TestPackageUpdatePreservesManagedBytecode(t *testing.T) {
	source := "github:example/policy"
	projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, source)
	initRepository(t, projectRoot)
	packageRoot := loader.packages[source].root
	sibling := filepath.Join(packageRoot, "skills", "skill0", "lib", "__pycache__", "sibling.pyc")
	asset := filepath.Join(packageRoot, "skills", "skill0", "lib", "__pycache__", "asset.pyc")
	writeFixture(t, sibling, []byte("retired bytecode\n"), 0o644)
	writeFixture(t, asset, []byte("managed bytecode\n"), 0o644)
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}

	stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
	}
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("package update = %d, %q, %q", code, stdout, stderr)
	}
	for _, root := range skillRoots("acr__example__policy__skill0") {
		content, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "__pycache__", "asset.pyc"))
		if err != nil || string(content) != "managed bytecode\n" {
			t.Errorf("retained managed bytecode in %s = %q, %v", root, content, err)
		}
		if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "__pycache__", "sibling.pyc")); !os.IsNotExist(err) {
			t.Errorf("removed sibling in %s = %v", root, err)
		}
	}
	stdout, stderr, code = runCLI(t, application, "check", "--project", projectRoot)
	if code != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Realization is current for ") {
		t.Fatalf("check after package update = %d, %q, %q", code, stdout, stderr)
	}
}

func TestRetiringSkillFilesPreservesTrackedBytecode(t *testing.T) {
	for _, action := range []string{"uninstall", "package-update"} {
		t.Run(action, func(t *testing.T) {
			source := "github:example/policy"
			projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, source)
			initRepository(t, projectRoot)
			cache := filepath.Join(loader.packages[source].root, "skills", "skill0", "lib", "__pycache__")
			writeFixture(t, filepath.Join(filepath.Dir(cache), "helper.py"), []byte("value = 42\n"), 0o644)
			writeFixture(t, filepath.Join(cache, "asset.pyc"), []byte("tracked bytecode\n"), 0o755)
			writeFixture(t, filepath.Join(cache, "sibling.pyc"), []byte("retired bytecode\n"), 0o644)
			application := &Application{service: service, fallback: cli.UnavailableApplication{}}
			stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
			}
			roots := skillRoots("acr__example__policy__skill0")
			gitArgs := []string{"-C", projectRoot, "add", "-f", "--"}
			for _, root := range roots {
				gitArgs = append(gitArgs, root+"/lib/__pycache__/asset.pyc")
			}
			if output, err := exec.Command("git", gitArgs...).CombinedOutput(); err != nil {
				t.Fatalf("track realized bytecode: %v: %s", err, output)
			}
			args := []string{"uninstall", source, "--project", projectRoot}
			if action == "package-update" {
				if err := os.RemoveAll(cache); err != nil {
					t.Fatal(err)
				}
				args = []string{"realize", "--project", projectRoot}
			}
			before := hashProjectTree(t, projectRoot)
			if action == "package-update" {
				for _, command := range [][]string{append(args, "--dry-run"), args} {
					stdout, stderr, code = runCLI(t, application, command...)
					if code != cli.ExitConflict || !strings.Contains(stderr, "tracked by Git and was dropped by an upgraded package") || !strings.Contains(stderr, "recorded managedHash") {
						t.Fatalf("tracked dropped bytecode = %d, %q, %q", code, stdout, stderr)
					}
					if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
						t.Fatal("tracked dropped bytecode refusal changed the project or ownership ledger")
					}
				}
				return
			}
			stdout, stderr, code = runCLI(t, application, append(args, "--dry-run")...)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("dry-run = %d, %q, %q", code, stdout, stderr)
			}
			if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
				t.Fatal("dry-run changed the project")
			}
			stdout, stderr, code = runCLI(t, application, args...)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("apply = %d, %q, %q", code, stdout, stderr)
			}
			ledger := loadLedger(t, projectRoot)
			for _, root := range roots {
				t.Run(root, func(t *testing.T) {
					asset := root + "/lib/__pycache__/asset.pyc"
					filename := filepath.Join(projectRoot, filepath.FromSlash(asset))
					content, err := os.ReadFile(filename)
					if err != nil || string(content) != "tracked bytecode\n" {
						t.Errorf("retained tracked bytecode = %q, %v", content, err)
					}
					if info, err := os.Stat(filename); err != nil || info.Mode().Perm() != 0o755 {
						t.Errorf("tracked bytecode mode: info = %v, err = %v; want 0755", info, err)
					}
					if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root), "lib", "__pycache__", "sibling.pyc")); !os.IsNotExist(err) {
						t.Errorf("retired sibling = %v, want removed", err)
					}
					for _, target := range ledger.Targets {
						if target.Path == asset {
							t.Errorf("retained ledger ownership of %s", asset)
						}
					}
				})
			}
			if output, err := exec.Command("git", "-C", projectRoot, "ls-files", "--deleted").CombinedOutput(); err != nil || len(output) != 0 {
				t.Errorf("tracked deletions = %q, %v; want none", output, err)
			}
			stdout, stderr, code = runCLI(t, application, "check", "--project", projectRoot)
			if code != cli.ExitSuccess || stderr != "" {
				t.Fatalf("check after apply = %d, %q, %q", code, stdout, stderr)
			}
		})
	}
}

func TestSkillRemovalPreservesUnownedBytecode(t *testing.T) {
	for _, history := range []string{"never-owned", "retired-ownership"} {
		for _, action := range []string{"package-update", "uninstall"} {
			for _, modified := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/modified=%t", history, action, modified), func(t *testing.T) {
					source := "github:example/policy"
					projectRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, source)
					initRepository(t, projectRoot)
					packageRoot := loader.packages[source].root
					for _, directory := range []string{"lib", "disposable"} {
						writeFixture(t, filepath.Join(packageRoot, "skills", "skill0", directory, "helper.py"), []byte("value = 42\n"), 0o644)
					}
					assets := []string{"__pycache__/runtime.pyc", "lib/__pycache__/runtime.pyc"}
					if history == "retired-ownership" {
						for _, asset := range assets {
							writeFixture(t, filepath.Join(packageRoot, "skills", "skill0", filepath.FromSlash(asset)), []byte("staged bytecode\n"), 0o755)
						}
					}
					application := &Application{service: service, fallback: cli.UnavailableApplication{}}
					stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
					if code != cli.ExitSuccess || stderr != "" {
						t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
					}
					roots := skillRoots("acr__example__policy__skill0")
					gitArgs := []string{"-C", projectRoot, "add", "-f", "--"}
					for _, root := range roots {
						for _, asset := range assets {
							if history == "never-owned" {
								writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(root+"/"+asset)), []byte("staged bytecode\n"), 0o755)
							}
							gitArgs = append(gitArgs, root+"/"+asset)
						}
					}
					if output, err := exec.Command("git", gitArgs...).CombinedOutput(); err != nil {
						t.Fatalf("force-add bytecode: %v: %s", err, output)
					}
					indexPath := filepath.Join(projectRoot, ".git", "index")
					indexBefore, err := os.ReadFile(indexPath)
					if err != nil {
						t.Fatal(err)
					}
					content, mode := "staged bytecode\n", os.FileMode(0o755)
					runtimeBefore := make(map[string]map[string]string)
					assertPreserved := func(t *testing.T) {
						t.Helper()
						ledger := loadLedger(t, projectRoot)
						for _, root := range roots {
							for _, asset := range assets {
								targetPath := root + "/" + asset
								filename := filepath.Join(projectRoot, filepath.FromSlash(targetPath))
								if got, err := os.ReadFile(filename); err != nil || string(got) != content {
									t.Errorf("%s bytes = %q, %v; want %q", targetPath, got, err, content)
								}
								if info, err := os.Stat(filename); err != nil || info.Mode().Perm() != mode {
									t.Errorf("%s mode: info = %v, err = %v; want %04o", targetPath, info, err, mode)
								}
								for _, target := range ledger.Targets {
									if target.Path == targetPath {
										t.Errorf("ledger still owns %s", targetPath)
									}
								}
							}
						}
						if got, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(got, indexBefore) {
							t.Errorf("Git index changed: %v", err)
						}
						for cache, want := range runtimeBefore {
							if got := hashProjectTree(t, cache); !reflect.DeepEqual(got, want) {
								t.Errorf("untracked cache bytes/modes changed in %s: got %v, want %v", cache, got, want)
							}
						}
						if output, err := exec.Command("git", "-C", projectRoot, "ls-files", "--deleted").CombinedOutput(); err != nil || len(output) != 0 {
							t.Errorf("indexed deletions = %q, %v; want none", output, err)
						}
					}
					if history == "retired-ownership" {
						for _, asset := range assets {
							if err := os.Remove(filepath.Join(packageRoot, "skills", "skill0", filepath.FromSlash(asset))); err != nil {
								t.Fatal(err)
							}
						}
						// Tracked managed assets retire through explicit uninstall;
						// dropping them in a package update must retain ownership.
						state, err := dependency.LoadState(projectRoot)
						if err != nil {
							t.Fatal(err)
						}
						stdout, stderr, code = runCLI(t, application, "uninstall", source, "--project", projectRoot)
						if code != cli.ExitSuccess || stderr != "" {
							t.Fatalf("retire ownership = %d, %q, %q", code, stdout, stderr)
						}
						assertPreserved(t)
						retired, err := dependency.LoadState(projectRoot)
						if err != nil {
							t.Fatal(err)
						}
						state.Lock.Realization = retired.Lock.Realization
						if err := dependency.WriteState(projectRoot, state); err != nil {
							t.Fatal(err)
						}
						stdout, stderr, code = runCLI(t, application, "realize", "--project", projectRoot)
						if code != cli.ExitSuccess || stderr != "" {
							t.Fatalf("reinstall after ownership retirement = %d, %q, %q", code, stdout, stderr)
						}
					}
					assertPreserved(t)
					if modified {
						content, mode = "unstaged bytecode edits\n", 0o640
						for _, root := range roots {
							for _, asset := range assets {
								writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(root+"/"+asset)), []byte(content), mode)
							}
						}
					}
					for _, root := range roots {
						cache := filepath.Join(projectRoot, filepath.FromSlash(root), "disposable", "__pycache__")
						writeFixture(t, filepath.Join(cache, "helper.pyc"), []byte("untracked cache\n"), 0o600)
						runtimeBefore[cache] = hashProjectTree(t, cache)
					}
					args := []string{"uninstall", source, "--project", projectRoot}
					if action == "package-update" {
						updated := loader.packages[source]
						updated.manifest.Artifacts.Skills = nil
						loader.packages[source] = updated
						args = []string{"realize", "--project", projectRoot}
					}
					before := hashProjectTree(t, projectRoot)
					stdout, stderr, code = runCLI(t, application, append(args, "--dry-run")...)
					if code != cli.ExitSuccess || stderr != "" {
						t.Fatalf("dry-run = %d, %q, %q", code, stdout, stderr)
					}
					if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
						t.Fatal("dry-run changed the project")
					}
					assertPreserved(t)
					stdout, stderr, code = runCLI(t, application, args...)
					if code != cli.ExitSuccess || stderr != "" {
						t.Fatalf("skill removal = %d, %q, %q", code, stdout, stderr)
					}
					assertPreserved(t)
					for _, root := range roots {
						for _, relative := range []string{"SKILL.md", "lib/helper.py", "disposable/helper.py"} {
							if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root+"/"+relative))); !os.IsNotExist(err) {
								t.Errorf("%s/%s = %v; want removed", root, relative, err)
							}
						}
					}
					stdout, stderr, code = runCLI(t, application, "check", "--project", projectRoot)
					if code != cli.ExitSuccess || stderr != "" {
						t.Fatalf("check after skill removal = %d, %q, %q", code, stdout, stderr)
					}
				})
			}
		}
	}
}

func plantRootBytecodeCache(t *testing.T, projectRoot, skillRoot string) {
	t.Helper()
	writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(skillRoot), "__pycache__", "skill.cpython-311.pyc"), []byte("pyc"), 0o644)
}

func assertRootBytecodeCaches(t *testing.T, projectRoot string, roots []string, want bool) {
	t.Helper()
	for _, root := range roots {
		_, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root), "__pycache__", "skill.cpython-311.pyc"))
		exists := err == nil
		if exists != want {
			t.Fatalf("%s/__pycache__/skill.cpython-311.pyc exists = %v, err = %v, want exists = %v", root, exists, err, want)
		}
	}
}

func TestSkillRemovalPreservesBytecodeInEnclosingGitRoot(t *testing.T) {
	for _, action := range []string{"package-update", "uninstall"} {
		for _, modified := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/modified=%t", action, modified), func(t *testing.T) {
				const source = "github:example/policy"
				fixtureRoot, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, source)
				gitRoot := t.TempDir()
				projectRoot := filepath.Join(gitRoot, "consumer")
				if err := os.Rename(fixtureRoot, projectRoot); err != nil {
					t.Fatal(err)
				}
				initRepository(t, gitRoot)
				if _, err := os.Lstat(filepath.Join(projectRoot, ".git")); !os.IsNotExist(err) {
					t.Fatalf("consumer Git metadata = %v, want enclosing root only", err)
				}
				writeFixture(t, filepath.Join(loader.packages[source].root, "skills", "skill0", "lib", "helper.py"), []byte("value = 42\n"), 0o644)
				application := &Application{service: service, fallback: cli.UnavailableApplication{}}
				stdout, stderr, code := runCLI(t, application, "realize", "--project", projectRoot)
				if code != cli.ExitSuccess || stderr != "" {
					t.Fatalf("initial realize = %d, %q, %q", code, stdout, stderr)
				}
				roots := skillRoots("acr__example__policy__skill0")
				assets := []string{"__pycache__/user.pyc", "lib/__pycache__/user.pyc"}
				gitArgs := []string{"-C", gitRoot, "add", "-f", "--"}
				for _, root := range roots {
					for _, asset := range assets {
						writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(root+"/"+asset)), []byte("indexed bytecode\n"), 0o755)
						gitArgs = append(gitArgs, "consumer/"+root+"/"+asset)
					}
				}
				if output, err := exec.Command("git", gitArgs...).CombinedOutput(); err != nil {
					t.Fatalf("force-add never-owned caches: %v: %s", err, output)
				}
				indexPath := filepath.Join(gitRoot, ".git", "index")
				indexBefore, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				content, mode := "indexed bytecode\n", os.FileMode(0o755)
				if modified {
					content, mode = "unstaged bytecode edits\n", 0o640
					for _, root := range roots {
						for _, asset := range assets {
							writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(root+"/"+asset)), []byte(content), mode)
						}
					}
				}
				assertPreserved := func() {
					t.Helper()
					ledger := loadLedger(t, projectRoot)
					for _, root := range roots {
						for _, asset := range assets {
							targetPath := root + "/" + asset
							filename := filepath.Join(projectRoot, filepath.FromSlash(targetPath))
							if got, err := os.ReadFile(filename); err != nil || string(got) != content {
								t.Errorf("%s bytes = %q, %v; want %q", targetPath, got, err, content)
							}
							if info, err := os.Stat(filename); err != nil || info.Mode().Perm() != mode {
								t.Errorf("%s mode: info = %v, err = %v; want %04o", targetPath, info, err, mode)
							}
							for _, target := range ledger.Targets {
								if target.Path == targetPath {
									t.Errorf("runtime cache entered ledger: %s", targetPath)
								}
							}
						}
					}
					if got, err := os.ReadFile(indexPath); err != nil || !bytes.Equal(got, indexBefore) {
						t.Errorf("enclosing Git index changed: %v", err)
					}
					if output, err := exec.Command("git", "-C", gitRoot, "ls-files", "--deleted").CombinedOutput(); err != nil || len(output) != 0 {
						t.Errorf("indexed deletions = %q, %v; want none", output, err)
					}
				}
				assertPreserved()
				args := []string{"uninstall", source, "--project", projectRoot}
				if action == "package-update" {
					updated := loader.packages[source]
					updated.manifest.Artifacts.Skills = nil
					loader.packages[source] = updated
					args = []string{"realize", "--project", projectRoot}
				}
				before := hashProjectTree(t, projectRoot)
				stdout, stderr, code = runCLI(t, application, append(args, "--dry-run")...)
				if code != cli.ExitSuccess || stderr != "" {
					t.Fatalf("dry-run = %d, %q, %q", code, stdout, stderr)
				}
				if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
					t.Fatal("dry-run changed consumer files")
				}
				assertPreserved()
				stdout, stderr, code = runCLI(t, application, args...)
				if code != cli.ExitSuccess || stderr != "" {
					t.Fatalf("skill removal = %d, %q, %q", code, stdout, stderr)
				}
				assertPreserved()
				for _, target := range loadLedger(t, projectRoot).Targets {
					for _, root := range roots {
						if strings.HasPrefix(target.Path, root+"/") {
							t.Errorf("removed skill ownership remains: %s", target.Path)
						}
					}
				}
				for _, root := range roots {
					for _, managed := range []string{"SKILL.md", "lib/helper.py", "run.sh", "references/notes.md"} {
						if _, err := os.Lstat(filepath.Join(projectRoot, filepath.FromSlash(root+"/"+managed))); !os.IsNotExist(err) {
							t.Errorf("managed %s/%s = %v; want absent", root, managed, err)
						}
					}
				}
				for _, command := range []string{"realize", "check"} {
					stdout, stderr, code = runCLI(t, application, command, "--project", projectRoot)
					if code != cli.ExitSuccess || stderr != "" {
						t.Fatalf("subsequent %s = %d, %q, %q", command, code, stdout, stderr)
					}
				}
				assertPreserved()
			})
		}
	}
}
