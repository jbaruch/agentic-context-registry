package realizeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	firstSource  = "github:example/first"
	secondSource = "github:example/second"
)

type fixturePackage struct {
	root     string
	manifest manifest.Manifest
}

// multiPackageLoader answers per declared source, which fixtureLoader cannot
// do, and counts every materialization so an offline claim stays honest.
type multiPackageLoader struct {
	packages map[string]fixturePackage
	failures map[string]error
	calls    int
}

func (loader *multiPackageLoader) MaterializeLocked(_ context.Context, locked dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	loader.calls++
	if err, failing := loader.failures[locked.Source]; failing {
		return dependency.MaterializedPackage{}, nil, err
	}
	value, known := loader.packages[locked.Source]
	if !known {
		return dependency.MaterializedPackage{}, nil, fmt.Errorf("no fixture package for %s", locked.Source)
	}
	return dependency.MaterializedPackage{Root: value.root, Manifest: value.manifest}, func() error { return nil }, nil
}

// uninstallFixture writes a project declaring one package per source, each
// contributing one always-on rule, without realizing it yet.
func uninstallFixture(t *testing.T, agents []string, sources ...string) (string, *multiPackageLoader, *Application) {
	t.Helper()
	return newUninstallFixture(t, agents, false, sources...)
}

// uninstallFixtureWithHooks adds a session-start hook to every package, so two
// packages contribute entries to one generated-only structured config target
// (.claude/settings.json) as well as to one generated-only host file.
func uninstallFixtureWithHooks(t *testing.T, agents []string, sources ...string) (string, *multiPackageLoader, *Application) {
	t.Helper()
	return newUninstallFixture(t, agents, true, sources...)
}

func newUninstallFixture(t *testing.T, agents []string, hooks bool, sources ...string) (string, *multiPackageLoader, *Application) {
	t.Helper()
	projectRoot := t.TempDir()
	loader := &multiPackageLoader{packages: map[string]fixturePackage{}, failures: map[string]error{}}
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: agents},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	for index, source := range sources {
		packageRoot := t.TempDir()
		writeFixture(t, filepath.Join(packageRoot, "rules", "guidance.md"), []byte("# Guidance from "+source+"\n"), 0o644)
		artifacts := manifest.Artifacts{
			Rules: []manifest.RuleArtifact{{ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
		}
		if hooks {
			writeFixture(t, filepath.Join(packageRoot, "hooks", "session.sh"), []byte("#!/bin/sh\necho "+source+"\n"), 0o755)
			artifacts.Hooks = []manifest.HookArtifact{{ID: "session", Event: manifest.HookSessionStart, Path: "hooks/session.sh"}}
		}
		loader.packages[source] = fixturePackage{root: packageRoot, manifest: manifest.Manifest{Artifacts: artifacts}}
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
	return projectRoot, loader, &Application{service: NewService(loader), fallback: cli.UnavailableApplication{}}
}

func realizeProject(t *testing.T, application *Application, projectRoot string) {
	t.Helper()
	stdout, stderr, exitCode := runCLI(t, application, "realize", "--project", projectRoot, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("fixture realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

// projectFiles hashes every file except the two dependency state files, which
// an uninstall is expected to rewrite.
func projectFiles(t *testing.T, projectRoot string) map[string]string {
	t.Helper()
	digests := hashProjectTree(t, projectRoot)
	delete(digests, dependency.ProjectFilename)
	delete(digests, dependency.LockFilename)
	return digests
}

func ledgerPaths(ledger realize.Ledger) map[string]struct{} {
	paths := make(map[string]struct{}, len(ledger.Targets))
	for _, target := range ledger.Targets {
		paths[target.Path] = struct{}{}
	}
	return paths
}

// gitExcludeFile is the one path outside the previous ledger an uninstall may
// write; the markers are the block delimiters internal/realize/git.go writes.
const (
	gitExcludeFile     = ".git/info/exclude"
	excludeBeginMarker = "# BEGIN ACR GENERATED OUTPUTS"
	excludeEndMarker   = "# END ACR GENERATED OUTPUTS"
)

// seededTimer pins the machine-local freshness record and the bytes it must
// still carry. The record is keyed by project and policy, never by package, so
// an uninstall has no business rewriting it.
type seededTimer struct {
	path    string
	content []byte
}

func freshnessTimer(t *testing.T, stateHome, projectRoot string) seededTimer {
	t.Helper()
	store := freshness.Store{BaseDirectory: stateHome}
	checkedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := store.Write(projectRoot, checkedAt, freshness.PolicyOutdated, freshness.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	statePath, _, err := store.Paths(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	return seededTimer{path: statePath, content: readFile(t, statePath)}
}

func (timer seededTimer) assertUnchanged(t *testing.T) {
	t.Helper()
	if after := readFile(t, timer.path); !bytes.Equal(after, timer.content) {
		t.Fatalf("freshness timer %q = %s, want %s", timer.path, after, timer.content)
	}
}

// initRepository makes the fixture a real initialized repository so the
// planner's Git inspection is live. A fake inspector disables exclusion
// planning entirely, which would hide every .git/info/exclude operation.
func initRepository(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("Git is required for realization integration tests; install Git and ensure it is on PATH: %v", err)
	}
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", root, err, output)
	}
}

// excludePatterns returns the patterns inside the ACR marker block of
// .git/info/exclude. It fails when the file is missing, because uninstall may
// prune the block's entries and must never remove the file.
func excludePatterns(t *testing.T, projectRoot string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(gitExcludeFile)))
	if err != nil {
		t.Fatalf("read %s: %v", gitExcludeFile, err)
	}
	body := string(content)
	begin := strings.Index(body, excludeBeginMarker)
	end := strings.Index(body, excludeEndMarker)
	if begin < 0 || end < begin {
		return nil
	}
	patterns := make([]string, 0, 4)
	for _, line := range strings.Split(body[begin+len(excludeBeginMarker):end], "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			patterns = append(patterns, trimmed)
		}
	}
	return patterns
}

// TestUninstallRemovesDeclarationLockHoldAndOutputs asserts the whole
// preserve half of its row, not only the removal half: the agent selection,
// the freshness policy, both files' unknown top-level fields, and the
// machine-local timer keyed by ACR_STATE_HOME all survive byte-identical.
//
// t.Setenv forbids t.Parallel, and the timer lives outside the project by
// design, so it can only be pinned to this test's temporary directory here.
func TestUninstallRemovesDeclarationLockHoldAndOutputs(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("ACR_STATE_HOME", stateHome)

	projectRoot, _, application := uninstallFixture(t, []string{"claude-code", "codex"}, firstSource, secondSource)
	seeded, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	seeded.Project.Freshness = "outdated"
	seeded.Project.Extra = map[string]any{"experimental": map[string]any{"owner": "someone"}}
	seeded.Lock.Extra = map[string]any{"generator": "acr-test"}
	if err := dependency.WriteState(projectRoot, seeded); err != nil {
		t.Fatal(err)
	}
	timer := freshnessTimer(t, stateHome, projectRoot)
	realizeProject(t, application, projectRoot)
	before := projectLedger(t, projectRoot)
	if len(ledgerPaths(before)) == 0 {
		t.Fatal("fixture realized no targets")
	}

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot)
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Removed "+firstSource+"@v1.0.0") {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 1 || state.Project.Dependencies[0].Source != secondSource {
		t.Fatalf("declarations after uninstall = %#v", state.Project.Dependencies)
	}
	if len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Source != secondSource {
		t.Fatalf("lock rows after uninstall = %#v", state.Lock.Dependencies)
	}
	if !reflect.DeepEqual(state.Project.Agents, []string{"claude-code", "codex"}) {
		t.Fatalf("agent selection changed: %#v", state.Project.Agents)
	}
	if state.Project.Freshness != "outdated" {
		t.Fatalf("freshness policy after uninstall = %q, want outdated", state.Project.Freshness)
	}
	if !reflect.DeepEqual(state.Project.Extra, seeded.Project.Extra) {
		t.Fatalf("project Extra after uninstall = %#v, want %#v", state.Project.Extra, seeded.Project.Extra)
	}
	if !reflect.DeepEqual(state.Lock.Extra, seeded.Lock.Extra) {
		t.Fatalf("lock Extra after uninstall = %#v, want %#v", state.Lock.Extra, seeded.Lock.Extra)
	}
	timer.assertUnchanged(t)
	after := projectLedger(t, projectRoot)
	for _, target := range after.Targets {
		for _, entry := range target.Entries {
			if entry.Source == firstSource {
				t.Fatalf("ledger still owns %s in %q", firstSource, target.Path)
			}
		}
	}
	for path := range ledgerPaths(before) {
		if _, kept := ledgerPaths(after)[path]; kept {
			continue
		}
		if _, err := os.Stat(filepath.Join(projectRoot, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dropped target %q survives on disk: %v", path, err)
		}
	}
}

// TestUninstallLeavesSiblingDependenciesByteIdentical asserts every byte the
// surviving package owns: its whole declaration and lock row including the
// unknown per-row fields another owner may have added, its ledger targets, and
// each of its realized outputs with its mode.
func TestUninstallLeavesSiblingDependenciesByteIdentical(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"cursor"}, firstSource, secondSource)
	seeded, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	for index := range seeded.Project.Dependencies {
		if seeded.Project.Dependencies[index].Source != secondSource {
			continue
		}
		seeded.Project.Dependencies[index].Extra = map[string]any{"reviewedBy": "someone"}
		seeded.Lock.Dependencies[index].Extra = map[string]any{"mirror": "cache.example"}
	}
	if err := dependency.WriteState(projectRoot, seeded); err != nil {
		t.Fatal(err)
	}
	realizeProject(t, application, projectRoot)
	declarationBefore, lockBefore := siblingRows(t, projectRoot)
	siblingTargets := siblingLedgerPaths(t, projectRoot)
	if len(siblingTargets) == 0 {
		t.Fatal("fixture realized no sibling-owned targets")
	}
	before := hashProjectTree(t, projectRoot)

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	declarationAfter, lockAfter := siblingRows(t, projectRoot)
	if !reflect.DeepEqual(declarationAfter, declarationBefore) {
		t.Fatalf("sibling declaration = %#v, want %#v", declarationAfter, declarationBefore)
	}
	if !reflect.DeepEqual(lockAfter, lockBefore) {
		t.Fatalf("sibling lock row = %#v, want %#v", lockAfter, lockBefore)
	}
	if got := siblingLedgerPaths(t, projectRoot); !reflect.DeepEqual(got, siblingTargets) {
		t.Fatalf("sibling ledger targets = %#v, want %#v", got, siblingTargets)
	}
	after := hashProjectTree(t, projectRoot)
	for _, path := range siblingTargets {
		if after[path] != before[path] {
			t.Fatalf("sibling output %q = %q, want %q", path, after[path], before[path])
		}
	}
}

// siblingRows returns the surviving package's declaration and lock row.
func siblingRows(t *testing.T, projectRoot string) (dependency.Declaration, dependency.LockedDependency) {
	t.Helper()
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	var declaration dependency.Declaration
	var locked dependency.LockedDependency
	for _, candidate := range state.Project.Dependencies {
		if candidate.Source == secondSource {
			declaration = candidate
		}
	}
	for _, candidate := range state.Lock.Dependencies {
		if candidate.Source == secondSource {
			locked = candidate
		}
	}
	if declaration.Source != secondSource || locked.Source != secondSource {
		t.Fatalf("%s is not declared and locked: %#v", secondSource, state)
	}
	return declaration, locked
}

// siblingLedgerPaths returns the sorted targets owned solely by the surviving
// package.
func siblingLedgerPaths(t *testing.T, projectRoot string) []string {
	t.Helper()
	var paths []string
	for _, target := range projectLedger(t, projectRoot).Targets {
		owned := len(target.Entries) != 0
		for _, entry := range target.Entries {
			if entry.Source != secondSource {
				owned = false
			}
		}
		if owned {
			paths = append(paths, target.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// TestUninstallKeepsSiblingPackageEntriesInASharedTarget covers both shapes of
// one generated-only target owned by two packages: the include lines in the
// codex host file and the two session-start hooks merged into the structured
// claude-code config. Each target must be rewritten down to the surviving
// package through an update or a merge, never removed and re-created.
func TestUninstallKeepsSiblingPackageEntriesInASharedTarget(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixtureWithHooks(t, []string{"claude-code", "codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	hosts := map[string][2]string{
		"AGENTS.md":             {firstSource, secondSource},
		".claude/settings.json": {"acr__example__first__session", "acr__example__second__session"},
	}
	for name, owners := range hosts {
		body := readFile(t, filepath.Join(projectRoot, filepath.FromSlash(name)))
		if !strings.Contains(string(body), owners[0]) || !strings.Contains(string(body), owners[1]) {
			t.Fatalf("fixture %s does not carry both packages: %s", name, body)
		}
	}

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	var payload struct {
		Result UninstallResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	ledger := projectLedger(t, projectRoot)

	for name, owners := range hosts {
		body := string(readFile(t, filepath.Join(projectRoot, filepath.FromSlash(name))))
		if strings.Contains(body, owners[0]) || !strings.Contains(body, owners[1]) {
			t.Fatalf("%s after uninstall = %s", name, body)
		}
		kind, planned := operationKind(payload.Result.Plan, name)
		if !planned || (kind != realize.OperationUpdate && kind != realize.OperationMerge) {
			t.Fatalf("%s was planned as %q (planned=%t), want an update or a merge", name, kind, planned)
		}
		sources := map[string]bool{}
		for _, target := range ledger.Targets {
			if target.Path != name {
				continue
			}
			for _, entry := range target.Entries {
				sources[entry.Source] = true
			}
		}
		if sources[firstSource] || !sources[secondSource] {
			t.Fatalf("%s ledger entries = %#v", name, sources)
		}
	}
}

func operationKind(plan realize.Plan, targetPath string) (realize.OperationKind, bool) {
	for _, operation := range plan.Operations {
		if operation.Path == targetPath {
			return operation.Kind, true
		}
	}
	return "", false
}

func TestUninstallPreservesOperatorContentInSharedTargets(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	operator := "# House rules\n\nAlways run the tests.\n"
	writeFixture(t, filepath.Join(projectRoot, "AGENTS.md"), []byte(operator), 0o644)
	realizeProject(t, application, projectRoot)

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	after, err := os.ReadFile(filepath.Join(projectRoot, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), operator) {
		t.Fatalf("operator content lost from the shared target: %s", after)
	}
	if strings.Contains(string(after), firstSource) || !strings.Contains(string(after), secondSource) {
		t.Fatalf("shared target after uninstall = %s", after)
	}
}

func TestUninstallCoversEveryLedgerAgentNotOnlyDeclaredAgents(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"claude-code", "codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Agents = []string{"codex"}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	claudeOwned := 0
	for _, target := range projectLedger(t, projectRoot).Targets {
		if strings.HasPrefix(target.Path, ".claude/") || target.Path == "CLAUDE.md" {
			claudeOwned++
		}
	}
	if claudeOwned == 0 {
		t.Fatal("fixture recorded no claude-code targets to cover")
	}

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "claude-code") {
		t.Fatalf("uninstall did not cover the ledger's claude-code targets: %q", stdout)
	}

	host, err := os.ReadFile(filepath.Join(projectRoot, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("claude-code host removed: %v", err)
	}
	if strings.Contains(string(host), firstSource) || !strings.Contains(string(host), secondSource) {
		t.Fatalf("claude-code host after uninstall = %s", host)
	}
	for _, target := range projectLedger(t, projectRoot).Targets {
		if !strings.HasPrefix(target.Path, ".claude/") && target.Path != "CLAUDE.md" {
			continue
		}
		if _, err := os.Stat(filepath.Join(projectRoot, filepath.FromSlash(target.Path))); err != nil {
			t.Fatalf("covered claude-code target %q is missing: %v", target.Path, err)
		}
		claudeOwned--
	}
	if claudeOwned != 0 {
		t.Fatalf("uninstall dropped %d claude-code target(s) from the ledger", claudeOwned)
	}
}

func TestUninstallNeverRemovesTheFreshnessHook(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, freshness.Source, secondSource)
	realizeProject(t, application, projectRoot)
	hook := filepath.Join(projectRoot, ".codex", "hooks", "acr__jbaruch__agentic-context-registry__freshness-session-start", "session-start.sh")
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("fixture realized no freshness hook: %v", err)
	}

	if _, stderr, exitCode := runCLI(t, application, "uninstall", freshness.Source, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("uninstall removed the freshness hook: %v", err)
	}
	owned := false
	for _, target := range projectLedger(t, projectRoot).Targets {
		for _, entry := range target.Entries {
			if entry.ArtifactID == freshness.ArtifactID {
				owned = true
			}
		}
	}
	if !owned {
		t.Fatal("uninstall dropped the freshness hook's ownership entries")
	}
}

// TestUninstallOnlyTouchesLedgerAndStatePaths runs in a real initialized
// repository, where Git inspection is live and the planner can emit its
// .git/info/exclude operation. Dependency state is part of the journal, and
// that Git metadata is the sole approved non-state exception to the
// previous-ledger path invariant. Every other path outside the previous ledger
// stays byte-identical.
func TestUninstallOnlyTouchesLedgerAndStatePaths(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex", "cursor"}, firstSource, secondSource)
	initRepository(t, projectRoot)
	realizeProject(t, application, projectRoot)
	unmanaged := map[string]string{
		".tessl/RULES.md":                "# Tessl rules\n",
		".agents/vendor/ws/pkg/rules.md": "# Vendored\n",
		"notes/operator.md":              "# Operator notes\n",
	}
	for name, content := range unmanaged {
		writeFixture(t, filepath.Join(projectRoot, filepath.FromSlash(name)), []byte(content), 0o644)
	}
	owned := ledgerPaths(projectLedger(t, projectRoot))
	before := projectFiles(t, projectRoot)

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	var payload struct {
		Result UninstallResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	excepted := false
	for _, operation := range payload.Result.Plan.Operations {
		if operation.Path == dependency.ProjectFilename || operation.Path == dependency.LockFilename {
			continue
		}
		if operation.Path == gitExcludeFile {
			excepted = true
			continue
		}
		if _, recorded := owned[operation.Path]; !recorded {
			t.Fatalf("uninstall planned %s on %q outside dependency state and the previous ledger", operation.Kind, operation.Path)
		}
	}
	// The approved exception must be live, not merely tolerated: a fixture that
	// never plans it would let the invariant pass without ever being tested.
	if !excepted {
		t.Fatalf("uninstall planned no %s operation, so the approved exception went untested: %#v", gitExcludeFile, payload.Result.Plan.Operations)
	}
	after := projectFiles(t, projectRoot)
	for name, digest := range before {
		if _, recorded := owned[name]; recorded || name == gitExcludeFile {
			continue
		}
		if after[name] != digest {
			t.Fatalf("unmanaged file %q = %q, want %q", name, after[name], digest)
		}
	}
}

// TestUninstallPrunesOnlyTheRemovedTargetFromTheGitExcludeBlock proves the
// approved exception in a real repository: the removed generated-only target
// loses its exclusion pattern, the surviving package keeps its own, the file
// still exists, and nothing outside the marker block changes.
func TestUninstallPrunesOnlyTheRemovedTargetFromTheGitExcludeBlock(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"cursor"}, firstSource, secondSource)
	initRepository(t, projectRoot)
	excludeFile := filepath.Join(projectRoot, filepath.FromSlash(gitExcludeFile))
	writeFixture(t, excludeFile, []byte("# operator exclusion\nbuild/\n"), 0o644)
	realizeProject(t, application, projectRoot)

	removed := "/.cursor/rules/acr__example__first__guidance.mdc"
	surviving := "/.cursor/rules/acr__example__second__guidance.mdc"
	patternsBefore := excludePatterns(t, projectRoot)
	if !contains(patternsBefore, removed) || !contains(patternsBefore, surviving) {
		t.Fatalf("exclusion block before uninstall = %#v, want both packages' targets", patternsBefore)
	}
	owned := ledgerPaths(projectLedger(t, projectRoot))
	before := projectFiles(t, projectRoot)

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	patternsAfter := excludePatterns(t, projectRoot)
	var want []string
	for _, pattern := range patternsBefore {
		if pattern != removed {
			want = append(want, pattern)
		}
	}
	if !reflect.DeepEqual(patternsAfter, want) {
		t.Fatalf("exclusion block after uninstall = %#v, want %#v", patternsAfter, want)
	}
	content, err := os.ReadFile(excludeFile)
	if err != nil {
		t.Fatalf("uninstall removed %s: %v", gitExcludeFile, err)
	}
	if !strings.Contains(string(content), "# operator exclusion\nbuild/\n") {
		t.Fatalf("uninstall rewrote operator exclusions: %s", content)
	}
	after := projectFiles(t, projectRoot)
	for name, digest := range before {
		if _, recorded := owned[name]; recorded || name == gitExcludeFile {
			continue
		}
		if after[name] != digest {
			t.Fatalf("path outside the previous ledger %q = %q, want %q", name, after[name], digest)
		}
	}
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func TestUninstallDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	before := hashProjectTree(t, projectRoot)

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--dry-run")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Would remove "+firstSource) {
		t.Fatalf("dry-run uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry-run uninstall wrote files:\n before %#v\n after  %#v", before, after)
	}

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall after dry run exit = %d, stderr = %q", exitCode, stderr)
	}
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 1 || state.Project.Dependencies[0].Source != secondSource {
		t.Fatalf("declarations after the applied uninstall = %#v", state.Project.Dependencies)
	}
}

func TestUninstallWritesNothingWhenStatePreparationFails(t *testing.T) {
	t.Parallel()

	projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	before := hashProjectTree(t, projectRoot)
	injected := errors.New("state preparation failed")
	failing := &Application{service: NewService(loader), fallback: cli.UnavailableApplication{}}
	failing.service.marshalState = func(dependency.State) ([]byte, []byte, error) {
		return nil, nil, injected
	}

	stdout, stderr, exitCode := runCLI(t, failing, "uninstall", firstSource, "--project", projectRoot, "--json")

	if exitCode != cli.ExitOperational || stdout != "" || !strings.Contains(stderr, "state preparation failed") {
		t.Fatalf("failed-preparation uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed state preparation left the project changed:\n before %#v\n after  %#v", before, after)
	}
}

func TestUninstallConflictsOnModifiedGeneratedOutput(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"cursor"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	target := filepath.Join(projectRoot, ".cursor", "rules", "acr__example__first__guidance.mdc")
	writeFixture(t, target, []byte("---\nalwaysApply: true\n---\n# Hand edited\n"), 0o644)
	before := hashProjectTree(t, projectRoot)

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")

	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
		t.Fatalf("conflicting uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("conflicting uninstall wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestUninstallFailsClosedWhenARemainingPackageIsUnavailable(t *testing.T) {
	t.Parallel()

	projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	before := hashProjectTree(t, projectRoot)
	loader.failures[secondSource] = errors.New("archive not found; verify repository access")

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")

	if exitCode != cli.ExitOperational || stdout != "" || !strings.Contains(stderr, `"code":"remaining_packages_unavailable"`) {
		t.Fatalf("offline uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "cannot re-render the 1 package(s)") || !strings.Contains(stderr, "acr uninstall "+firstSource) {
		t.Fatalf("offline diagnostic = %q, want the count and the retry command", stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("offline uninstall wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestUninstallWithoutRemainingPackagesMakesNoNetworkCall(t *testing.T) {
	t.Parallel()

	projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource)
	realizeProject(t, application, projectRoot)
	loader.calls = 0
	loader.failures[firstSource] = errors.New("remote must not be reached")

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("last-dependency uninstall exit = %d, stderr = %q", exitCode, stderr)
	}
	if loader.calls != 0 {
		t.Fatalf("last-dependency uninstall materialized %d package(s), want 0", loader.calls)
	}
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 {
		t.Fatalf("state after the last uninstall = %#v", state)
	}
}

func TestUninstallRefusesAnUndeclaredSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		source         string
		wantCode       string
		wantDiagnostic string
	}{
		{name: "never declared", source: "github:owner/missing", wantCode: cli.CodeDependencyNotDeclared, wantDiagnostic: "acr list"},
		{name: "undeclared vendor source", source: "vendor:workspace/package", wantCode: cli.CodeDependencyNotDeclared, wantDiagnostic: "acr list"},
		{name: "uppercase owner", source: "github:Owner/plugin", wantCode: "usage", wantDiagnostic: "github:owner/repository"},
		{name: "bare repository", source: "owner/plugin", wantCode: "usage", wantDiagnostic: "github:owner/repository"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource)
			before := hashProjectTree(t, projectRoot)

			stdout, stderr, exitCode := runCLI(t, application, "uninstall", test.source, "--project", projectRoot, "--json")

			if exitCode != cli.ExitUsage || stdout != "" {
				t.Fatalf("uninstall %s exit = %d, stdout = %q, stderr = %q", test.source, exitCode, stdout, stderr)
			}
			if !strings.Contains(stderr, `"code":"`+test.wantCode+`"`) || !strings.Contains(stderr, test.wantDiagnostic) {
				t.Fatalf("uninstall %s stderr = %q, want %q and %q", test.source, stderr, test.wantCode, test.wantDiagnostic)
			}
			if loader.calls != 0 {
				t.Fatalf("refused uninstall materialized %d package(s), want 0", loader.calls)
			}
			if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused uninstall wrote files:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

func TestUninstallSecondRunWritesNothing(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("first uninstall exit = %d, stderr = %q", exitCode, stderr)
	}
	before := hashProjectTree(t, projectRoot)

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")

	if exitCode != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, `"code":"`+cli.CodeDependencyNotDeclared+`"`) {
		t.Fatalf("second uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("second uninstall wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestUninstallJSONEnvelope(t *testing.T) {
	t.Parallel()

	for _, dryRun := range []bool{true, false} {
		dryRun := dryRun
		t.Run(fmt.Sprintf("dryRun=%t", dryRun), func(t *testing.T) {
			t.Parallel()

			projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
			realizeProject(t, application, projectRoot)
			args := []string{"uninstall", firstSource, "--project", projectRoot, "--json"}
			if dryRun {
				args = append(args, "--dry-run")
			}

			stdout, stderr, exitCode := runCLI(t, application, args...)

			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			var envelope struct {
				OK      bool            `json:"ok"`
				Command string          `json:"command"`
				Result  UninstallResult `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode %q: %v", stdout, err)
			}
			if !envelope.OK || envelope.Command != "uninstall" || !envelope.Result.Changed {
				t.Fatalf("uninstall envelope = %#v", envelope)
			}
			if envelope.Result.Source != firstSource || envelope.Result.Removed == nil || envelope.Result.Removed.Tag != "v1.0.0" {
				t.Fatalf("uninstall result = %#v", envelope.Result)
			}
			if !reflect.DeepEqual(envelope.Result.Agents, []string{"codex"}) {
				t.Fatalf("covered agents = %#v", envelope.Result.Agents)
			}
			if len(envelope.Result.Plan.Operations) == 0 {
				t.Fatalf("uninstall plan carries no operations: %#v", envelope.Result.Plan)
			}
			if strings.Contains(stdout, "Guidance from") {
				t.Fatalf("uninstall envelope leaked rendered file bodies: %q", stdout)
			}
		})
	}
}

func TestUninstallDropsTheRollbackHoldAndItsResumeBarrier(t *testing.T) {
	t.Parallel()

	projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	index := -1
	for position, declaration := range state.Project.Dependencies {
		if declaration.Source == firstSource {
			index = position
		}
	}
	if index < 0 || state.Lock.Dependencies[index].Source != firstSource {
		t.Fatalf("fixture does not declare %s at a matching lock row: %#v", firstSource, state)
	}
	state.Project.Dependencies[index].Hold = &dependency.Hold{Pin: "v1.0.0", Rejected: "v2.0.0"}
	state.Lock.Dependencies[index].Hold = &dependency.LockHold{RejectedTag: "v2.0.0"}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		t.Fatal(err)
	}
	realizeProject(t, application, projectRoot)

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("held uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	loaded, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatalf("pruned state does not validate: %v", err)
	}
	for _, declaration := range loaded.Project.Dependencies {
		if declaration.Source == firstSource || declaration.Hold != nil {
			t.Fatalf("hold survived the uninstall: %#v", declaration)
		}
	}
	for _, locked := range loaded.Lock.Dependencies {
		if locked.Source == firstSource || locked.Hold != nil {
			t.Fatalf("lock hold survived the uninstall: %#v", locked)
		}
	}
	resume := &Application{service: NewService(loader), fallback: dependency.NewApplication(&noRemote{})}
	stdout, stderr, exitCode := runCLI(t, resume, "resume", firstSource, "--project", projectRoot, "--json")
	if exitCode != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, `"code":"`+cli.CodeDependencyNotDeclared+`"`) {
		t.Fatalf("resume after uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

// noRemote fails every GitHub call so a refusal that must precede the network
// cannot pass by reaching it.
type noRemote struct{}

func (noRemote) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{}, errors.New("remote must not be called")
}

func (noRemote) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	return dependency.Release{}, errors.New("remote must not be called")
}

func (noRemote) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return "", errors.New("remote must not be called")
}

func (noRemote) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, errors.New("remote must not be called")
}

func (noRemote) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("remote must not be called")
}
