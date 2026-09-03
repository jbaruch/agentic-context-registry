package realizeapp

import (
	"errors"
	"os"
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

const (
	hostileRemovedSource = "github:hostile/removed"
	hostileSiblingSource = "github:hostile/sibling"
)

type hostileUninstallProject struct {
	root        string
	stateHome   string
	loader      *multiPackageLoader
	application *Application
}

func newHostileUninstallProject(t *testing.T, removedSource string) hostileUninstallProject {
	t.Helper()
	root := t.TempDir()
	stateHome := t.TempDir()
	loader := &multiPackageLoader{packages: map[string]fixturePackage{}, failures: map[string]error{}}
	for _, source := range []string{removedSource, hostileSiblingSource} {
		packageRoot := t.TempDir()
		writeFixture(t, filepath.Join(packageRoot, "rules", "policy.md"), []byte("# Policy from "+source+"\n"), 0o644)
		writeFixture(t, filepath.Join(packageRoot, "hooks", "boot.sh"), []byte("#!/bin/sh\nprintf '%s\\n' "+source+"\n"), 0o755)
		loader.packages[source] = fixturePackage{
			root: packageRoot,
			manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
				Rules: []manifest.RuleArtifact{{
					ID: "policy", Path: "rules/policy.md",
					Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways},
				}},
				Hooks: []manifest.HookArtifact{{ID: "boot", Event: manifest.HookSessionStart, Path: "hooks/boot.sh"}},
			}},
		}
	}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Agents:        []string{"claude-code", "codex"},
			Freshness:     "outdated",
			Extra:         map[string]any{"hostileOwner": "keep-project-extra"},
			Dependencies: []dependency.Declaration{
				{
					Source: removedSource, Requested: "latest",
					Hold: &dependency.Hold{Pin: "v1.0.0", Rejected: "v2.0.0"},
				},
				{Source: hostileSiblingSource, Requested: "latest", Extra: map[string]any{"keep": "declaration-extra"}},
			},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Extra:         map[string]any{"hostileLockOwner": "keep-lock-extra"},
			Dependencies: []dependency.LockedDependency{
				{
					Source: removedSource, Requested: "latest", Kind: dependency.ResolutionRelease,
					ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0",
					ContentHash: "sha256:" + strings.Repeat("a", 64),
					Hold: &dependency.LockHold{
						RejectedTag: "v2.0.0", RejectedReleaseID: 2, RejectedCommit: strings.Repeat("b", 40),
					},
				},
				{
					Source: hostileSiblingSource, Requested: "latest", Kind: dependency.ResolutionRelease,
					ReleaseID: 3, Tag: "v3.0.0", Commit: strings.Repeat("c", 40), PackageVersion: "3.0.0",
					ContentHash: "sha256:" + strings.Repeat("c", 64), Extra: map[string]any{"keep": "lock-extra"},
				},
			},
		},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(loader), fallback: cli.UnavailableApplication{}}
	realizeProject(t, application, root)
	writeFixture(t, filepath.Join(root, ".tessl", "plugins", "foreign", "RULES.md"), []byte("tessl-owned\n"), 0o640)
	writeFixture(t, filepath.Join(root, ".agents", "vendor", "foreign", "package", "rule.md"), []byte("vendored\n"), 0o600)
	writeFixture(t, filepath.Join(stateHome, "freshness", "timer.json"), []byte("{\"attempt\":\"keep\"}\n"), 0o600)
	return hostileUninstallProject{root: root, stateHome: stateHome, loader: loader, application: application}
}

func hostileEntriesForSource(ledger realize.Ledger, source string) map[string][]realize.Entry {
	entries := map[string][]realize.Entry{}
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			if entry.Source == source {
				entries[target.Path] = append(entries[target.Path], entry)
			}
		}
	}
	return entries
}

func hostileReadFile(t *testing.T, filename string) []byte {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestHostileUninstallPrunesOneHeldPackageAndPreservesEveryOtherOwner(t *testing.T) {
	project := newHostileUninstallProject(t, hostileRemovedSource)
	t.Setenv("ACR_STATE_HOME", project.stateHome)

	beforeTree := hashProjectTree(t, project.root)
	beforeTimer := hashProjectTree(t, project.stateHome)
	beforeState, err := dependency.LoadState(project.root)
	if err != nil {
		t.Fatal(err)
	}
	beforeLedger := projectLedger(t, project.root)
	siblingEntries := hostileEntriesForSource(beforeLedger, hostileSiblingSource)
	if len(siblingEntries) == 0 {
		t.Fatal("fixture has no sibling ledger entries")
	}
	for _, path := range []string{".claude/settings.json", "AGENTS.md"} {
		if len(siblingEntries[path]) == 0 {
			t.Fatalf("fixture has no sibling entry in shared target %s", path)
		}
	}

	stdout, stderr, exitCode := runCLI(t, project.application, "uninstall", hostileRemovedSource, "--project", project.root, "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"command":"uninstall"`) {
		t.Fatalf("dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, project.root); !reflect.DeepEqual(after, beforeTree) {
		t.Fatalf("dry-run changed the project:\n before %#v\n after  %#v", beforeTree, after)
	}

	stdout, stderr, exitCode = runCLI(t, project.application, "uninstall", hostileRemovedSource, "--project", project.root, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"changed":true`) {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	afterState, err := dependency.LoadState(project.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterState.Project.Dependencies) != 1 || afterState.Project.Dependencies[0].Source != hostileSiblingSource {
		t.Fatalf("remaining declarations = %#v", afterState.Project.Dependencies)
	}
	if len(afterState.Lock.Dependencies) != 1 || afterState.Lock.Dependencies[0].Source != hostileSiblingSource {
		t.Fatalf("remaining locks = %#v", afterState.Lock.Dependencies)
	}
	if !reflect.DeepEqual(afterState.Project.Dependencies[0], beforeState.Project.Dependencies[1]) ||
		!reflect.DeepEqual(afterState.Lock.Dependencies[0], beforeState.Lock.Dependencies[1]) {
		t.Fatalf("sibling state changed:\n before %#v / %#v\n after  %#v / %#v",
			beforeState.Project.Dependencies[1], beforeState.Lock.Dependencies[1],
			afterState.Project.Dependencies[0], afterState.Lock.Dependencies[0])
	}
	if !reflect.DeepEqual(afterState.Project.Agents, beforeState.Project.Agents) ||
		afterState.Project.Freshness != beforeState.Project.Freshness ||
		!reflect.DeepEqual(afterState.Project.Extra, beforeState.Project.Extra) ||
		!reflect.DeepEqual(afterState.Lock.Extra, beforeState.Lock.Extra) {
		t.Fatalf("uninstall changed selections or extension fields: %#v", afterState)
	}
	afterLedger := projectLedger(t, project.root)
	if removed := hostileEntriesForSource(afterLedger, hostileRemovedSource); len(removed) != 0 {
		t.Fatalf("removed package still owns ledger entries: %#v", removed)
	}
	if got := hostileEntriesForSource(afterLedger, hostileSiblingSource); !reflect.DeepEqual(got, siblingEntries) {
		t.Fatalf("sibling ledger entries changed:\n before %#v\n after  %#v", siblingEntries, got)
	}
	claudeSettings := hostileReadFile(t, filepath.Join(project.root, ".claude", "settings.json"))
	agents := hostileReadFile(t, filepath.Join(project.root, "AGENTS.md"))
	if bytesContainAny(claudeSettings, hostileRemovedSource, "hostile__removed__boot") ||
		bytesContainAny(agents, hostileRemovedSource, "hostile__removed__policy") {
		t.Fatalf("shared targets retain removed package bytes:\n%s\n%s", claudeSettings, agents)
	}
	if !bytesContainAny(claudeSettings, "hostile__sibling__boot") || !bytesContainAny(agents, hostileSiblingSource) {
		t.Fatalf("shared targets lost sibling bytes:\n%s\n%s", claudeSettings, agents)
	}
	if after := hashProjectTree(t, project.stateHome); !reflect.DeepEqual(after, beforeTimer) {
		t.Fatalf("freshness timer changed:\n before %#v\n after  %#v", beforeTimer, after)
	}
	for _, path := range []string{".tessl/plugins/foreign/RULES.md", ".agents/vendor/foreign/package/rule.md"} {
		if hashProjectTree(t, project.root)[path] != beforeTree[path] {
			t.Fatalf("unmanaged tree path %s changed", path)
		}
	}
	stdout, stderr, exitCode = runCLI(t, project.application, "check", "--project", project.root, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"ok":true`) {
		t.Fatalf("check after uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	stable := hashProjectTree(t, project.root)
	stdout, stderr, exitCode = runCLI(t, project.application, "uninstall", hostileRemovedSource, "--project", project.root, "--json")
	if exitCode != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, `"code":"dependency_not_declared"`) {
		t.Fatalf("second uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, project.root); !reflect.DeepEqual(after, stable) {
		t.Fatalf("second uninstall changed the tree:\n before %#v\n after  %#v", stable, after)
	}
	for _, source := range []string{"vendor:ws/pkg", "github:Owner/x"} {
		stdout, stderr, exitCode = runCLI(t, project.application, "uninstall", source, "--project", project.root, "--json")
		if exitCode != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, "github:owner/repository") {
			t.Fatalf("uninstall %s exit = %d, stdout = %q, stderr = %q", source, exitCode, stdout, stderr)
		}
		if after := hashProjectTree(t, project.root); !reflect.DeepEqual(after, stable) {
			t.Fatalf("refusal for %s changed the tree", source)
		}
	}
}

func bytesContainAny(content []byte, values ...string) bool {
	for _, value := range values {
		if strings.Contains(string(content), value) {
			return true
		}
	}
	return false
}

func TestHostileUninstallOfFreshnessSourceKeepsThePolicyHook(t *testing.T) {
	project := newHostileUninstallProject(t, freshness.Source)
	before := hostileEntriesForSource(projectLedger(t, project.root), freshness.Source)
	if len(before) == 0 {
		t.Fatal("fixture records no entries under the freshness source")
	}

	_, stderr, exitCode := runCLI(t, project.application, "uninstall", freshness.Source, "--project", project.root)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}
	after := hostileEntriesForSource(projectLedger(t, project.root), freshness.Source)
	policyEntry := false
	packageEntry := false
	for _, entries := range after {
		for _, entry := range entries {
			policyEntry = policyEntry || entry.ArtifactID == freshness.ArtifactID
			packageEntry = packageEntry || entry.ArtifactID == "policy" || entry.ArtifactID == "boot"
		}
	}
	if !policyEntry || packageEntry {
		t.Fatalf("freshness-source entries after uninstall = %#v", after)
	}
	hook := filepath.Join(project.root, ".claude", "hooks", "acr__jbaruch__agentic-context-registry__freshness-session-start", "session-start.sh")
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("freshness policy hook was removed: %v", err)
	}
}

func TestHostileUninstallFailuresLeaveStateAndNativeTreesByteIdentical(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*testing.T, hostileUninstallProject) *Application
		wantExit int
		wantCode string
	}{
		{
			name: "remaining archive missing",
			prepare: func(_ *testing.T, project hostileUninstallProject) *Application {
				project.loader.failures[hostileSiblingSource] = errors.New("archive missing")
				return project.application
			},
			wantExit: cli.ExitOperational,
			wantCode: "remaining_packages_unavailable",
		},
		{
			name: "hand edited generated file",
			prepare: func(t *testing.T, project hostileUninstallProject) *Application {
				path := ""
				for _, target := range projectLedger(t, project.root).Targets {
					entries := hostileEntriesForSource(realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{target}}, hostileRemovedSource)
					sibling := hostileEntriesForSource(realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{target}}, hostileSiblingSource)
					if len(entries[target.Path]) != 0 && len(sibling[target.Path]) == 0 && target.Ownership == realize.OwnershipGenerated {
						path = target.Path
						break
					}
				}
				if path == "" {
					t.Fatal("fixture has no generated removed-package rule")
				}
				writeFixture(t, filepath.Join(project.root, filepath.FromSlash(path)), []byte("hand edited\n"), 0o644)
				return project.application
			},
			wantExit: cli.ExitConflict,
			wantCode: "realization_conflict",
		},
		{
			name: "state preparation failure",
			prepare: func(t *testing.T, project hostileUninstallProject) *Application {
				failing := &Application{service: NewService(project.loader), fallback: cli.UnavailableApplication{}}
				failing.service.marshalState = func(dependency.State) ([]byte, []byte, error) {
					return nil, nil, errors.New("injected state preparation failure")
				}
				return failing
			},
			wantExit: cli.ExitOperational,
			wantCode: "realization_failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := newHostileUninstallProject(t, hostileRemovedSource)
			application := test.prepare(t, project)
			before := hashProjectTree(t, project.root)

			stdout, stderr, exitCode := runCLI(t, application, "uninstall", hostileRemovedSource, "--project", project.root, "--json")
			if exitCode != test.wantExit || stdout != "" || !strings.Contains(stderr, `"code":"`+test.wantCode+`"`) {
				t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if after := hashProjectTree(t, project.root); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed uninstall changed yaml, lock, or native files:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

func TestHostileAgentSubsetCarriesClaudeOutputsAndLedgerThroughACodexRerender(t *testing.T) {
	project := newHostileUninstallProject(t, hostileRemovedSource)
	beforeTree := hashProjectTree(t, project.root)
	beforeClaude := claudeCodeTargets(t, beforeTree)
	beforeLedger := adapterTargets(projectLedger(t, project.root), "claude-code")
	writeFixture(t, filepath.Join(project.loader.packages[hostileSiblingSource].root, "rules", "policy.md"), []byte("# Revised sibling policy\n"), 0o644)

	stdout, stderr, exitCode := runCLI(t, project.application, "realize", "--project", project.root, "--agent", "codex", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"agents":["codex"]`) {
		t.Fatalf("subset realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	for path, digest := range beforeClaude {
		if got := hashProjectTree(t, project.root)[path]; got != digest {
			t.Fatalf("omitted Claude output %s changed: got %q want %q", path, got, digest)
		}
	}
	if got := adapterTargets(projectLedger(t, project.root), "claude-code"); !reflect.DeepEqual(got, beforeLedger) {
		t.Fatalf("omitted Claude ledger changed:\n before %#v\n after  %#v", beforeLedger, got)
	}
	stdout, stderr, exitCode = runCLI(t, project.application, "check", "--project", project.root, "--agent", "codex", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"ok":true`) {
		t.Fatalf("subset check exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
}

func TestHostileMixedAdapterLedgerFailsClosedBeforeWriting(t *testing.T) {
	project := newHostileUninstallProject(t, hostileRemovedSource)
	state, err := dependency.LoadState(project.root)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	mixed := false
	for index := range ledger.Targets {
		if ledger.Targets[index].Path != "AGENTS.md" || len(ledger.Targets[index].Entries) == 0 {
			continue
		}
		entry := ledger.Targets[index].Entries[0]
		entry.Adapter = "claude-code"
		entry.ArtifactID += "-mixed"
		ledger.Targets[index].Entries = append(ledger.Targets[index].Entries, entry)
		mixed = true
		break
	}
	if !mixed {
		t.Fatal("fixture has no AGENTS.md entry to make mixed")
	}
	encoded, err := realize.EncodeLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	state.Lock.Realization = encoded
	if err := dependency.WriteState(project.root, state); err != nil {
		t.Fatal(err)
	}
	before := hashProjectTree(t, project.root)

	stdout, stderr, exitCode := runCLI(t, project.application, "realize", "--project", project.root, "--agent", "codex", "--json")
	if exitCode != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) ||
		!strings.Contains(stderr, "re-run without --agent") {
		t.Fatalf("mixed-ledger realize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, project.root); !reflect.DeepEqual(after, before) {
		t.Fatalf("mixed-ledger refusal changed the tree:\n before %#v\n after  %#v", before, after)
	}
}

func TestHostileLastDependencyUninstallsWithoutMaterialization(t *testing.T) {
	root, loader, application := uninstallFixture(t, []string{"codex"}, hostileRemovedSource)
	realizeProject(t, application, root)
	loader.calls = 0
	loader.failures[hostileRemovedSource] = errors.New("network must remain unused")

	_, stderr, exitCode := runCLI(t, application, "uninstall", hostileRemovedSource, "--project", root)
	if exitCode != cli.ExitSuccess || stderr != "" || loader.calls != 0 {
		t.Fatalf("last uninstall exit = %d, stderr = %q, materializations = %d", exitCode, stderr, loader.calls)
	}
}
