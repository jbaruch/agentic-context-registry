package realizeapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

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
	projectRoot := t.TempDir()
	loader := &multiPackageLoader{packages: map[string]fixturePackage{}, failures: map[string]error{}}
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Agents: agents},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	for index, source := range sources {
		packageRoot := t.TempDir()
		writeFixture(t, filepath.Join(packageRoot, "rules", "guidance.md"), []byte("# Guidance from "+source+"\n"), 0o644)
		loader.packages[source] = fixturePackage{root: packageRoot, manifest: manifest.Manifest{Artifacts: manifest.Artifacts{
			Rules: []manifest.RuleArtifact{{ID: "guidance", Path: "rules/guidance.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}},
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

func TestUninstallRemovesDeclarationLockHoldAndOutputs(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"claude-code", "codex"}, firstSource, secondSource)
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

func TestUninstallLeavesSiblingDependenciesByteIdentical(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"cursor"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	sibling := filepath.Join(projectRoot, ".cursor", "rules", "acr__example__second__guidance.mdc")
	before, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatal(err)
	}

	if _, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot); exitCode != cli.ExitSuccess {
		t.Fatalf("uninstall exit = %d, stderr = %q", exitCode, stderr)
	}

	after, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatalf("sibling rule removed: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("sibling rule = %q, want %q", after, before)
	}
	state, err := dependency.LoadState(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Source != secondSource {
		t.Fatalf("sibling lock row changed: %#v", state.Lock.Dependencies)
	}
}

func TestUninstallKeepsSiblingPackageEntriesInASharedTarget(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	host := filepath.Join(projectRoot, "AGENTS.md")
	before, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), firstSource) || !strings.Contains(string(before), secondSource) {
		t.Fatalf("fixture host does not carry both packages: %s", before)
	}

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", firstSource, "--project", projectRoot, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}

	after, err := os.ReadFile(host)
	if err != nil {
		t.Fatalf("shared host removed: %v", err)
	}
	if strings.Contains(string(after), firstSource) || !strings.Contains(string(after), secondSource) {
		t.Fatalf("shared host after uninstall = %s", after)
	}
	var payload struct {
		Result UninstallResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	for _, operation := range payload.Result.Plan.Operations {
		if operation.Path == "AGENTS.md" && operation.Kind == realize.OperationRemove {
			t.Fatalf("shared host was removed rather than updated: %#v", operation)
		}
	}
	sources := map[string]bool{}
	for _, target := range projectLedger(t, projectRoot).Targets {
		if target.Path != "AGENTS.md" {
			continue
		}
		for _, entry := range target.Entries {
			sources[entry.Source] = true
		}
	}
	if sources[firstSource] || !sources[secondSource] {
		t.Fatalf("AGENTS.md ledger entries = %#v", sources)
	}
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

func TestUninstallOnlyTouchesPathsInThePreviousLedger(t *testing.T) {
	t.Parallel()

	projectRoot, _, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
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
	for _, operation := range payload.Result.Plan.Operations {
		if _, recorded := owned[operation.Path]; !recorded {
			t.Fatalf("uninstall planned %s on %q, which the previous ledger does not own", operation.Kind, operation.Path)
		}
	}
	after := projectFiles(t, projectRoot)
	for name, digest := range before {
		if _, recorded := owned[name]; recorded {
			continue
		}
		if after[name] != digest {
			t.Fatalf("unmanaged file %q = %q, want %q", name, after[name], digest)
		}
	}
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

func TestUninstallRollsBackWhenStateWriteFails(t *testing.T) {
	t.Parallel()

	projectRoot, loader, application := uninstallFixture(t, []string{"codex"}, firstSource, secondSource)
	realizeProject(t, application, projectRoot)
	before := hashProjectTree(t, projectRoot)
	injected := errors.New("lock replacement failed")
	failing := &Application{service: NewService(loader), fallback: cli.UnavailableApplication{}}
	failing.service.writeState = func(string, dependency.State) error { return injected }

	stdout, stderr, exitCode := runCLI(t, failing, "uninstall", firstSource, "--project", projectRoot, "--json")

	if exitCode != cli.ExitOperational || stdout != "" || !strings.Contains(stderr, "lock replacement failed") {
		t.Fatalf("failed-write uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "rolled back") {
		t.Fatalf("failed-write diagnostic = %q, want a rollback statement", stderr)
	}
	if after := hashProjectTree(t, projectRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed uninstall left the project changed:\n before %#v\n after  %#v", before, after)
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
		{name: "vendor source", source: "vendor:workspace/package", wantCode: "usage", wantDiagnostic: "github:owner/repository"},
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
