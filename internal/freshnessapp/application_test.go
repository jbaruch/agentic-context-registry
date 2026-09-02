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
)

func TestFreshnessCLIJSONFailOpenTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		policy      freshness.Policy
		code        string
		exitCode    int
		wantState   bool
		wantOutcome freshness.Outcome
		configure   func(*testing.T, string, freshness.Store) (*Runner, func())
	}{
		{
			name: "offline", policy: freshness.PolicyOutdated, code: CodeOffline, exitCode: cli.ExitOperational,
			wantState: true, wantOutcome: freshness.OutcomeOffline,
			configure: func(_ *testing.T, _ string, store freshness.Store) (*Runner, func()) {
				checker := &fakeOutdatedChecker{err: &dependency.RemoteError{Err: errors.New("network unreachable")}}
				return NewRunner(store, func() time.Time { return runnerNow }, checker), func() {}
			},
		},
		{
			name: "authentication", policy: freshness.PolicyOutdated, code: CodeAuth, exitCode: cli.ExitOperational,
			wantState: true, wantOutcome: freshness.OutcomeAuth,
			configure: func(_ *testing.T, _ string, store freshness.Store) (*Runner, func()) {
				checker := &fakeOutdatedChecker{err: &dependency.RemoteError{StatusCode: 401, Err: errors.New("access denied")}}
				return NewRunner(store, func() time.Time { return runnerNow }, checker), func() {}
			},
		},
		{
			name: "lock contention", policy: freshness.PolicyOutdated, code: CodeBusy, exitCode: cli.ExitSuccess,
			configure: func(t *testing.T, project string, store freshness.Store) (*Runner, func()) {
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
			name: "update failure", policy: freshness.PolicyInstall, code: CodeUpdateFailed, exitCode: cli.ExitOperational,
			wantState: true, wantOutcome: freshness.OutcomeFailed,
			configure: func(t *testing.T, project string, store freshness.Store) (*Runner, func()) {
				writeFreshnessProjectState(t, project)
				runner := NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{})
				return runner.WithInstall(&fakeReconciler{err: errors.New("reconcile failed")}, &fakeRealizer{}), func() {}
			},
		},
		{
			name: "preservation conflict", policy: freshness.PolicyInstall, code: CodeConflict, exitCode: cli.ExitConflict,
			wantState: true, wantOutcome: freshness.OutcomeConflict,
			configure: func(t *testing.T, project string, store freshness.Store) (*Runner, func()) {
				writeFreshnessProjectState(t, project)
				runner := NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{})
				return runner.WithInstall(&fakeReconciler{}, &fakeRealizer{err: &realize.ConflictError{}}), func() {}
			},
		},
		{
			name: "state unwritable", policy: freshness.PolicyOutdated, code: CodeStateUnwritable, exitCode: cli.ExitOperational,
			configure: func(_ *testing.T, _ string, store freshness.Store) (*Runner, func()) {
				runner := NewRunner(store, func() time.Time { return runnerNow }, &fakeOutdatedChecker{})
				runner.write = func(freshness.Store, string, time.Time, freshness.Policy, freshness.Outcome) error {
					return errors.New("read-only state volume")
				}
				return runner, func() {}
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			store := freshness.Store{BaseDirectory: t.TempDir()}
			runner, cleanup := test.configure(t, project, store)
			defer cleanup()
			application := &Application{runner: runner, fallback: cli.UnavailableApplication{}}
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{
				"freshness", "run", "--project", project, "--policy", string(test.policy), "--json",
			})

			if exitCode != test.exitCode {
				t.Fatalf("exit code = %d, want %d; stdout = %q, stderr = %q", exitCode, test.exitCode, stdout.String(), stderr.String())
			}
			if stdout.Len() == 0 || strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
				t.Fatalf("stdout = %q, want exactly one JSON line", stdout.String())
			}
			var envelope struct {
				OK      bool   `json:"ok"`
				Command string `json:"command"`
				Result  struct {
					Notices []cli.Notice `json:"notices"`
				} `json:"result"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("decode JSON stdout %q: %v", stdout.String(), err)
			}
			if envelope.OK != (test.exitCode == cli.ExitSuccess) || envelope.Command != "freshness" || len(envelope.Result.Notices) != 1 || envelope.Result.Notices[0].Code != test.code {
				t.Fatalf("envelope = %#v, want %s notice", envelope, test.code)
			}
			if !strings.Contains(stderr.String(), test.code+": ") {
				t.Fatalf("stderr = %q, want %s notice", stderr.String(), test.code)
			}
			state, usable, err := store.Read(project)
			if err != nil {
				t.Fatal(err)
			}
			if usable != test.wantState {
				t.Fatalf("freshness state usable = %t, want %t; state = %#v", usable, test.wantState, state)
			}
			if test.wantState && (state.LastCheckedAt != runnerNow || state.LastOutcome != test.wantOutcome || state.LastPolicy != test.policy) {
				t.Fatalf("freshness state = %#v", state)
			}
		})
	}
}

func TestApplicationReportsDefaultStoreConstructionFailure(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	project := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Freshness: "install"},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exitCode := runFreshnessCLI(t, NewApplication(offlineGitHub{}), project, "", true)

	if exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Policy  freshness.Policy `json:"policy"`
			Notices []cli.Notice     `json:"notices"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("JSON stdout %q: %v", stdout, err)
	}
	if envelope.OK || envelope.Result.Policy != freshness.PolicyInstall || len(envelope.Result.Notices) != 1 || envelope.Result.Notices[0].Code != CodeStateUnwritable {
		t.Fatalf("envelope = %#v, want stored install policy and %s", envelope, CodeStateUnwritable)
	}
	if !strings.Contains(stderr, CodeStateUnwritable+": ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestApplicationRecordsProjectStateLoadFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		policy       string
		exitCode     int
		wantOK       bool
		wantPolicy   freshness.Policy
		wantThrottle bool
		wantDiagnose string
		configure    func(*testing.T, string)
	}{
		{
			name:     "malformed agents.yaml",
			exitCode: cli.ExitOperational, wantPolicy: freshness.PolicyOutdated,
			wantThrottle: true, wantDiagnose: "acr outdated",
			configure: func(t *testing.T, project string) {
				writeProjectFile(t, project, dependency.ProjectFilename, "schemaVersion: [\n")
			},
		},
		{
			name:     "unreachable agents.yaml",
			exitCode: cli.ExitOperational, wantPolicy: freshness.PolicyOutdated,
			wantThrottle: true, wantDiagnose: "acr outdated",
			configure: func(t *testing.T, project string) {
				if err := os.Mkdir(filepath.Join(project, dependency.ProjectFilename), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "malformed agents.yaml with policy none",
			policy: "none", exitCode: cli.ExitSuccess, wantOK: true, wantPolicy: freshness.PolicyNone,
			wantDiagnose: "acr outdated",
			configure: func(t *testing.T, project string) {
				writeProjectFile(t, project, dependency.ProjectFilename, "schemaVersion: [\n")
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			project := t.TempDir()
			test.configure(t, project)
			store := freshness.Store{BaseDirectory: t.TempDir()}
			service := dependency.NewService(dependency.NewResolver(offlineGitHub{}))
			application := &Application{
				runner:   NewRunner(store, func() time.Time { return runnerNow }, service),
				fallback: cli.UnavailableApplication{},
			}

			stdout, stderr, exitCode := runFreshnessCLI(t, application, project, test.policy, true)

			if exitCode != test.exitCode || strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Fatalf("first run exit = %d stdout = %q stderr = %q, want %d", exitCode, stdout, stderr, test.exitCode)
			}
			var envelope struct {
				OK     bool `json:"ok"`
				Result struct {
					Policy  freshness.Policy `json:"policy"`
					Notices []cli.Notice     `json:"notices"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("decode JSON stdout %q: %v", stdout, err)
			}
			if envelope.OK != test.wantOK || envelope.Result.Policy != test.wantPolicy || len(envelope.Result.Notices) != 1 || envelope.Result.Notices[0].Code != CodeUpdateFailed {
				t.Fatalf("first envelope = %#v, want ok=%t policy=%s one %s notice", envelope, test.wantOK, test.wantPolicy, CodeUpdateFailed)
			}
			if !strings.Contains(stderr, CodeUpdateFailed+": ") {
				t.Fatalf("first stderr = %q, want an actionable %s notice", stderr, CodeUpdateFailed)
			}
			if test.wantDiagnose != "" && !strings.Contains(stderr, test.wantDiagnose) {
				t.Fatalf("first stderr = %q, want %q", stderr, test.wantDiagnose)
			}
			if !test.wantThrottle {
				return
			}
			state, usable, err := store.Read(project)
			if err != nil || !usable || state.LastCheckedAt != runnerNow || state.LastPolicy != freshness.PolicyOutdated || state.LastOutcome != freshness.OutcomeFailed {
				t.Fatalf("recorded state = %#v, usable = %t, error = %v", state, usable, err)
			}

			application.runner = NewRunner(store, func() time.Time { return runnerNow.Add(time.Second) }, service)
			stdout, stderr, exitCode = runFreshnessCLI(t, application, project, test.policy, true)
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("second run exit = %d stdout = %q stderr = %q", exitCode, stdout, stderr)
			}
			var throttled struct {
				OK     bool `json:"ok"`
				Result struct {
					Throttled bool `json:"throttled"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &throttled); err != nil || !throttled.OK || !throttled.Result.Throttled {
				t.Fatalf("second envelope = %#v, error = %v; want a throttled success", throttled, err)
			}
		})
	}
}
