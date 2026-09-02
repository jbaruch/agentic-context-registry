package freshnessapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

func TestFollowupsProjectStateFailuresEmitJSONNoticeAndThrottleWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*testing.T) string
	}{
		{
			name: "malformed agents.yaml",
			configure: func(t *testing.T) string {
				project := t.TempDir()
				writeProjectFile(t, project, dependency.ProjectFilename, "schemaVersion: [\n")
				return project
			},
		},
		{
			name: "unreadable agents.yaml mode 000",
			configure: func(t *testing.T) string {
				project := t.TempDir()
				path := filepath.Join(project, dependency.ProjectFilename)
				if err := os.WriteFile(path, []byte("schemaVersion: 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(path, 0o644); err != nil {
						t.Errorf("restore %s mode: %v", path, err)
					}
				})
				return project
			},
		},
		{
			name: "missing project root",
			configure: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := test.configure(t)
			store := freshness.Store{BaseDirectory: t.TempDir()}
			service := dependency.NewService(dependency.NewResolver(offlineGitHub{}))
			application := &Application{
				runner:   NewRunner(store, func() time.Time { return runnerNow }, service),
				fallback: cli.UnavailableApplication{},
			}

			stdout, stderr, exitCode := runFreshnessCLI(t, application, project, "", true)

			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d stdout=%q stderr=%q, want operational fail-open", exitCode, stdout, stderr)
			}
			if strings.TrimSpace(stdout) == "" {
				t.Fatalf("stdout empty, want one JSON envelope; stderr=%q", stderr)
			}
			if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Fatalf("stdout = %q, want exactly one JSON line", stdout)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("stderr empty, want an actionable notice; stdout=%q", stdout)
			}
			var envelope struct {
				OK      bool   `json:"ok"`
				Command string `json:"command"`
				Result  struct {
					Notices []cli.Notice `json:"notices"`
				} `json:"result"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("stdout is not a JSON envelope %q: %v; stderr=%q", stdout, err, stderr)
			}
			if envelope.Error != nil {
				t.Fatalf("JSON error envelope on stdout = %#v; want a result envelope. stderr=%q", envelope.Error, stderr)
			}
			if envelope.OK || envelope.Command != "freshness" || len(envelope.Result.Notices) != 1 {
				t.Fatalf("envelope = %#v, want one fail-open freshness notice", envelope)
			}
			notice := envelope.Result.Notices[0]
			if notice.Code == "" || notice.Message == "" {
				t.Fatalf("notice = %#v, want an actionable code and message", notice)
			}
			if !strings.Contains(stderr, notice.Code+": ") {
				t.Fatalf("stderr = %q, want the %s notice on the diagnostic stream", stderr, notice.Code)
			}
			state, usable, err := store.Read(project)
			if err != nil {
				t.Fatalf("throttle-state read error = %v; envelope=%#v stderr=%q", err, envelope, stderr)
			}
			if !usable || state.LastCheckedAt != runnerNow || state.LastOutcome == "" {
				t.Fatalf("throttle state = %#v, usable = %t; want a recorded attempt", state, usable)
			}
		})
	}
}

func TestFollowupsFutureLastCheckedAtRunsAndRewritesInjectedClock(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	store := freshness.Store{BaseDirectory: t.TempDir()}
	if err := store.Write(project, runnerNow.Add(time.Hour), freshness.PolicyOutdated, freshness.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	checker := &fakeOutdatedChecker{}
	runner := NewRunner(store, func() time.Time { return runnerNow }, checker)

	result, err := runner.Run(context.Background(), project, freshness.PolicyOutdated)
	if err != nil || result.Throttled || checker.calls != 1 {
		t.Fatalf("Run() = %#v, %v, calls = %d; want one unthrottled check against the injected clock", result, err, checker.calls)
	}
	state, usable, err := store.Read(project)
	if err != nil || !usable || state.LastCheckedAt != runnerNow || state.LastPolicy != freshness.PolicyOutdated {
		t.Fatalf("rewritten state = %#v, usable = %t, error = %v", state, usable, err)
	}
}
