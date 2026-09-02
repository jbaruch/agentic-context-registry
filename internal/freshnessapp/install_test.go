package freshnessapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

type sessionHoldPolicy struct {
	calls int
}

func (policy *sessionHoldPolicy) Resolve(context.Context, dependency.Declaration, *dependency.LockedDependency, dependency.Release) (dependency.HoldDecision, error) {
	policy.calls++
	return dependency.HoldDecision{Skip: true, Notice: "Held known-good release."}, nil
}

type heldGitHub struct {
	candidate     string
	downloadCalls int
}

func (github *heldGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{ID: 2, Tag: "v2.0.0"}, nil
}

func (github *heldGitHub) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	return dependency.Release{}, errors.New("unexpected exact release lookup")
}

func (github *heldGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return github.candidate, nil
}

func (github *heldGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	github.downloadCalls++
	return nil, errors.New("held release must not download")
}

func (github *heldGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("held release must not download metadata")
}

type fakeReconciler struct {
	calls  int
	dryRun bool
	err    error
}

func (reconciler *fakeReconciler) Reconcile(_ context.Context, _ string, dryRun bool) (dependency.ChangeResult, error) {
	reconciler.calls++
	reconciler.dryRun = dryRun
	return dependency.ChangeResult{Changed: true}, reconciler.err
}

type fakeRealizer struct {
	calls  int
	mode   realize.Mode
	result realizeapp.Result
	err    error
}

func (realizer *fakeRealizer) Run(_ context.Context, _ string, _ []string, mode realize.Mode) (realizeapp.Result, error) {
	realizer.calls++
	realizer.mode = mode
	return realizer.result, realizer.err
}

func TestInstallModeReconcilesThenRealizesTransactionally(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	writeFreshnessProjectState(t, project)
	reconciler := &fakeReconciler{}
	realizer := &fakeRealizer{result: realizeapp.Result{
		Agents: []string{"codex"},
		Plan:   realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationUpdate, Path: ".codex/skills/example/SKILL.md"}}},
	}}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(reconciler, realizer)
	result, err := runner.Run(context.Background(), project, freshness.PolicyInstall)
	if err != nil {
		t.Fatal(err)
	}
	if reconciler.calls != 1 || reconciler.dryRun || realizer.calls != 1 || realizer.mode != realize.ModeApply {
		t.Fatalf("reconcile calls = %d, realize calls = %d, mode = %q", reconciler.calls, realizer.calls, realizer.mode)
	}
	if !result.RestartRequired || len(result.Agents) != 1 || result.Agents[0] != "codex" || len(result.RestartAgents) != 1 || result.RestartAgents[0] != "codex" || result.Notices[0].Code != CodeRestartRequired {
		t.Fatalf("result = %#v", result)
	}
}

func TestInstallModeRestartRequiredFromPlan(t *testing.T) {
	t.Parallel()

	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion, Targets: []realize.Target{{
		Path: "AGENTS.md", Entries: []realize.Entry{{Adapter: "codex"}},
	}}}
	tests := []struct {
		name string
		plan realize.Plan
		want []string
	}{
		{name: "hook config", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationMerge, Path: ".claude/settings.json"}}}, want: []string{"claude-code"}},
		{name: "package content", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationUpdate, Path: ".cursor/skills/example/SKILL.md"}}}, want: []string{"cursor"}},
		{name: "shared removal", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationRemove, Path: "AGENTS.md"}}}, want: []string{"codex"}},
		{name: "unchanged", plan: realize.Plan{}, want: nil},
		{name: "ledger only", plan: realize.Plan{LedgerChanged: true}, want: nil},
		{name: "preserve only", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationPreserve, Path: ".codex/config.toml"}}}, want: nil},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := affectedAgents(test.plan, previous)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("affectedAgents() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestInstallModeClassifiesUpdateAndConflictFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reconcile  error
		realizeErr error
		code       string
		exit       int
		outcome    freshness.Outcome
	}{
		{name: "update", reconcile: errors.New("reconcile failed"), code: CodeUpdateFailed, exit: 1, outcome: freshness.OutcomeFailed},
		{name: "conflict", realizeErr: &realize.ConflictError{}, code: CodeConflict, exit: 4, outcome: freshness.OutcomeConflict},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			writeFreshnessProjectState(t, project)
			store := freshness.Store{BaseDirectory: t.TempDir()}
			reconciler := &fakeReconciler{err: test.reconcile}
			realizer := &fakeRealizer{err: test.realizeErr}
			runner := NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(reconciler, realizer)
			result, err := runner.Run(context.Background(), project, freshness.PolicyInstall)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Code != test.code || runErr.ExitCode != test.exit {
				t.Fatalf("Run() = %#v, %v", result, err)
			}
			if len(result.Notices) != 1 || result.Notices[0].Code != test.code {
				t.Fatalf("notices = %#v", result.Notices)
			}
			state, usable, readErr := store.Read(project)
			if readErr != nil || !usable || state.LastOutcome != test.outcome || state.LastCheckedAt != runnerNow {
				t.Fatalf("state = %#v, usable = %t, error = %v", state, usable, readErr)
			}
		})
	}
}

func TestInstallModeSurfacesRealizationNotices(t *testing.T) {
	t.Parallel()

	executor := installExecutor{
		reconciler: &fakeReconciler{},
		realizer: &fakeRealizer{result: realizeapp.Result{Notices: []adapter.Notice{{
			Code: "shared_file_requires_commit", Message: "Commit AGENTS.md.",
		}}}},
	}
	project := t.TempDir()
	writeFreshnessProjectState(t, project)
	result, err := executor.execute(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notices) != 1 || result.Notices[0].Code != "shared_file_requires_commit" {
		t.Fatalf("notices = %#v", result.Notices)
	}
}

func TestSessionStartInstallDoesNotReinstallHeldRejectedRelease(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	current := strings.Repeat("a", 40)
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "install",
			Dependencies: []dependency.Declaration{{Source: "github:owner/plugin", Requested: "latest"}},
		},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: dependency.ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: current, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64),
		}}},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(dependency.LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	remote := &heldGitHub{candidate: strings.Repeat("b", 40)}
	holds := &sessionHoldPolicy{}
	service := dependency.NewServiceWithHoldPolicy(dependency.NewResolver(remote), holds)
	realizer := &fakeRealizer{}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(service, realizer)
	result, err := runner.Run(context.Background(), project, freshness.PolicyInstall)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(dependency.LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || remote.downloadCalls != 0 || holds.calls != 1 || realizer.calls != 1 {
		t.Fatalf("result = %#v, archive calls = %d, hold calls = %d, realize calls = %d", result, remote.downloadCalls, holds.calls, realizer.calls)
	}
	if len(result.Notices) != 1 || result.Notices[0].Message != "Held known-good release." {
		t.Fatalf("notices = %#v", result.Notices)
	}
}

func writeFreshnessProjectState(t *testing.T, root string) {
	t.Helper()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "install"},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
}
