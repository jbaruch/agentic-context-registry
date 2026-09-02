package freshnessapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

func TestHostileCLIJSONFailOpenTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policy   freshness.Policy
		code     string
		exitCode int
		setup    func(*testing.T, string, freshness.Store) (*Runner, func())
	}{
		{
			name: "offline", policy: freshness.PolicyOutdated, code: CodeOffline, exitCode: cli.ExitOperational,
			setup: func(_ *testing.T, _ string, store freshness.Store) (*Runner, func()) {
				return NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{err: &dependency.RemoteError{Err: errors.New("network unreachable")}}), func() {}
			},
		},
		{
			name: "unauthorized", policy: freshness.PolicyOutdated, code: CodeAuth, exitCode: cli.ExitOperational,
			setup: func(_ *testing.T, _ string, store freshness.Store) (*Runner, func()) {
				return NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{err: &dependency.RemoteError{StatusCode: 401, Err: errors.New("GitHub access denied")}}), func() {}
			},
		},
		{
			name: "lockHeld", policy: freshness.PolicyOutdated, code: CodeBusy, exitCode: cli.ExitSuccess,
			setup: func(t *testing.T, project string, store freshness.Store) (*Runner, func()) {
				held, err := store.TryLock(project)
				if err != nil {
					t.Fatal(err)
				}
				return NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}), func() {
					if err := held.Close(); err != nil {
						t.Error(err)
					}
				}
			},
		},
		{
			name: "updateFailure", policy: freshness.PolicyInstall, code: CodeUpdateFailed, exitCode: cli.ExitOperational,
			setup: func(t *testing.T, project string, store freshness.Store) (*Runner, func()) {
				writeFreshnessProjectState(t, project)
				return NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).WithInstall(&fakeReconciler{err: errors.New("reconcile failed")}, &fakeRealizer{}), func() {}
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			runner, cleanup := test.setup(t, project, store)
			defer cleanup()
			stdout, stderr, exitCode := runFreshnessCLI(t, &Application{runner: runner, fallback: cli.UnavailableApplication{}}, project, string(test.policy), true)
			if exitCode != test.exitCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", exitCode, test.exitCode, stdout, stderr)
			}
			if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Fatalf("stdout = %q, want exactly one JSON line", stdout)
			}
			var envelope struct {
				OK     bool `json:"ok"`
				Result struct {
					Notices []cli.Notice `json:"notices"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("JSON stdout %q: %v", stdout, err)
			}
			if !envelope.OK || len(envelope.Result.Notices) != 1 || envelope.Result.Notices[0].Code != test.code {
				t.Fatalf("envelope = %#v, want notice %s", envelope, test.code)
			}
			if !strings.Contains(stderr, test.code+": ") {
				t.Fatalf("stderr = %q, want %s diagnostic", stderr, test.code)
			}
		})
	}
}

func TestHostileRunnerThrottleBoundariesAndPolicySwitch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		now       time.Time
		policy    freshness.Policy
		prior     freshness.Policy
		wantCalls int
		throttled bool
	}{
		{name: "minusOneSecond", now: runnerNow.Add(freshness.Window - time.Second), policy: freshness.PolicyOutdated, prior: freshness.PolicyOutdated, throttled: true},
		{name: "exactlyTwentyFourHours", now: runnerNow.Add(freshness.Window), policy: freshness.PolicyOutdated, prior: freshness.PolicyOutdated, wantCalls: 1},
		{name: "plusOneSecond", now: runnerNow.Add(freshness.Window + time.Second), policy: freshness.PolicyOutdated, prior: freshness.PolicyOutdated, wantCalls: 1},
		{name: "policySwitch", now: runnerNow.Add(time.Second), policy: freshness.PolicyOutdated, prior: freshness.PolicyInstall, wantCalls: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			if err := store.Write(project, runnerNow, test.prior, freshness.OutcomeOK); err != nil {
				t.Fatal(err)
			}
			checker := &fakeOutdatedChecker{}
			result, err := NewRunner(store, func() time.Time { return test.now }, checker).Run(context.Background(), project, test.policy)
			if err != nil {
				t.Fatal(err)
			}
			if checker.calls != test.wantCalls || result.Throttled != test.throttled {
				t.Fatalf("calls = %d, throttled = %t; want %d, %t", checker.calls, result.Throttled, test.wantCalls, test.throttled)
			}
		})
	}
}

func TestHostileRunnerFutureLastCheckedAtIsNotThrottled(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := freshness.Store{BaseDirectory: t.TempDir()}
	if err := store.Write(project, runnerNow.Add(time.Hour), freshness.PolicyOutdated, freshness.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	checker := &fakeOutdatedChecker{}
	result, err := NewRunner(store, func() time.Time { return runnerNow }, checker).Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if result.Throttled || checker.calls != 1 {
		t.Fatalf("future lastCheckedAt throttled the run: throttled=%t calls=%d", result.Throttled, checker.calls)
	}
}

func TestHostileCLIFreshnessRunUsesStoredPolicyWhenFlagOmitted(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "install"},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	checker := &fakeOutdatedChecker{}
	reconciler := &fakeReconciler{}
	realizer := &fakeRealizer{}
	runner := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, checker).WithInstall(reconciler, realizer)
	stdout, stderr, exitCode := runFreshnessCLI(t, &Application{runner: runner, fallback: cli.UnavailableApplication{}}, project, "", true)
	if exitCode != cli.ExitSuccess {
		t.Fatalf("exit = %d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if reconciler.calls != 1 || checker.calls != 0 {
		t.Fatalf("stored install policy invoked outdated=%d install=%d; agents.yaml must be the source of truth when --policy is omitted", checker.calls, reconciler.calls)
	}
}

func TestHostileRunnerSymlinkSharesThrottleAndTwoProjectsDoNot(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	realRoot := filepath.Join(parent, "project")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	store := freshness.Store{BaseDirectory: t.TempDir()}
	if err := store.Write(realRoot, runnerNow, freshness.PolicyOutdated, freshness.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	checker := &fakeOutdatedChecker{}
	result, err := NewRunner(store, func() time.Time { return runnerNow.Add(freshness.Window - time.Second) }, checker).Run(context.Background(), alias, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Throttled || checker.calls != 0 {
		t.Fatalf("symlink spelling did not share throttle: %#v calls=%d", result, checker.calls)
	}

	other := t.TempDir()
	otherChecker := &fakeOutdatedChecker{}
	otherResult, err := NewRunner(store, func() time.Time { return runnerNow.Add(freshness.Window - time.Second) }, otherChecker).Run(context.Background(), other, freshness.PolicyOutdated)
	if err != nil {
		t.Fatal(err)
	}
	if otherResult.Throttled || otherChecker.calls != 1 {
		t.Fatalf("distinct project was silenced by another project's timer: %#v calls=%d", otherResult, otherChecker.calls)
	}
}

func TestHostileInstallModeRestartRequiredThroughRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		plan        realize.Plan
		wantRestart bool
		wantAgent   string
		wantNotice  bool
	}{
		{name: "hook-config", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationMerge, Path: ".claude/settings.json"}}}, wantRestart: true, wantAgent: "claude-code", wantNotice: true},
		{name: "package-content", plan: realize.Plan{Operations: []realize.Operation{{Kind: realize.OperationUpdate, Path: ".cursor/skills/example/SKILL.md"}}}, wantRestart: true, wantAgent: "cursor", wantNotice: true},
		{name: "unchanged", plan: realize.Plan{}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			writeFreshnessProjectState(t, project)
			realizer := &fakeRealizer{result: realizeapp.Result{Agents: []string{"claude-code", "cursor"}, Plan: test.plan}}
			result, err := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).
				WithInstall(&fakeReconciler{}, realizer).
				Run(context.Background(), project, freshness.PolicyInstall)
			if err != nil {
				t.Fatal(err)
			}
			if result.RestartRequired != test.wantRestart {
				t.Fatalf("restart = %t, want %t; result=%#v", result.RestartRequired, test.wantRestart, result)
			}
			if test.wantAgent != "" && (len(result.Agents) != 1 || result.Agents[0] != test.wantAgent) {
				t.Fatalf("agents = %#v, want [%s]", result.Agents, test.wantAgent)
			}
			hasRestart := false
			for _, notice := range result.Notices {
				if notice.Code == CodeRestartRequired {
					hasRestart = true
				}
			}
			if hasRestart != test.wantNotice {
				t.Fatalf("restart notice present = %t, want %t; notices=%#v", hasRestart, test.wantNotice, result.Notices)
			}
		})
	}
}

func TestHostileInstallModeHoldSkipThroughRunner(t *testing.T) {
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
	result, err := NewRunner(freshness.Store{BaseDirectory: t.TempDir()}, func() time.Time { return runnerNow }, &fakeOutdatedChecker{}).
		WithInstall(service, &fakeRealizer{}).
		Run(context.Background(), project, freshness.PolicyInstall)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(dependency.LockFilename)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || remote.downloadCalls != 0 || holds.calls != 1 {
		t.Fatalf("hold skip leaked a write: result=%#v downloads=%d holds=%d", result, remote.downloadCalls, holds.calls)
	}
}

func TestHostileRunnerCorruptAndNewerStateConverge(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "truncated", content: `{"schemaVersion":`},
		{name: "newerSchema", content: `{"schemaVersion":99}` + "\n"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			statePath, _, err := store.Paths(project)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			checker := &fakeOutdatedChecker{}
			if _, err := NewRunner(store, func() time.Time { return runnerNow }, checker).Run(context.Background(), project, freshness.PolicyOutdated); err != nil {
				t.Fatal(err)
			}
			state, usable, err := store.Read(project)
			if err != nil || !usable || state.SchemaVersion != freshness.StateSchemaVersion || checker.calls != 1 {
				t.Fatalf("state=%#v usable=%t err=%v calls=%d", state, usable, err, checker.calls)
			}
		})
	}
}

type offlineGitHub struct{}

func (offlineGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{}, &dependency.RemoteError{Err: errors.New("network unreachable")}
}

func (offlineGitHub) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	return dependency.Release{}, errors.New("unexpected ReleaseByTag")
}

func (offlineGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return "", errors.New("unexpected ResolveCommit")
}

func (offlineGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, errors.New("unexpected DownloadArchive")
}

func TestHostileApplicationFreshnessRunOfflineJSON(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("ACR_STATE_HOME", stateHome)
	project := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "outdated",
			Dependencies: []dependency.Declaration{{Source: "github:owner/plugin", Requested: "latest"}},
		},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: "github:owner/plugin", Requested: "latest", Kind: dependency.ResolutionRelease,
			ReleaseID: 1, Tag: "v1.0.0", Commit: strings.Repeat("a", 40), PackageVersion: "1.0.0",
			ContentHash: "sha256:" + strings.Repeat("a", 64),
		}}},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exitCode := runFreshnessCLI(t, NewApplication(offlineGitHub{}), project, "outdated", true)
	if exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("stdout = %q, want one JSON line", stdout)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Notices []cli.Notice `json:"notices"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("JSON stdout %q: %v", stdout, err)
	}
	if !envelope.OK || len(envelope.Result.Notices) != 1 || envelope.Result.Notices[0].Code != CodeOffline {
		t.Fatalf("envelope = %#v", envelope)
	}
	if !strings.Contains(stderr, CodeOffline+": ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func runFreshnessCLI(t *testing.T, application cli.Application, project, policy string, jsonOut bool) (string, string, int) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args := []string{"freshness", "run", "--project", project}
	if policy != "" {
		args = append(args, "--policy", policy)
	}
	if jsonOut {
		args = append(args, "--json")
	}
	exitCode := cli.New(&stdout, &stderr, application, "test").Run(context.Background(), args)
	return stdout.String(), stderr.String(), exitCode
}
