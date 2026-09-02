package freshnessapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

			exitCode := cli.New(&stdout, &stderr, application, "test").Run(context.Background(), []string{
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
