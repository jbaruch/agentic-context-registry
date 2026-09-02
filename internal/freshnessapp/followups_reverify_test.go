package freshnessapp

import (
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

// TestReverifyUnusableProjectRootStaysOneEnvelopeWithoutStateHomeBlame covers the
// three ways --project can name something that is not a usable project tree. All
// three must produce one result envelope on stdout, one freshness_update_failed
// notice that names the path and --project, and no state-home blame. The two
// roots that cannot be canonicalized record nothing, because there is no project
// identity to key a throttle record on. A regular file does canonicalize, so it
// reaches the same fail-open project-state path a malformed agents.yaml takes and
// records its failed attempt.
func TestReverifyUnusableProjectRootStaysOneEnvelopeWithoutStateHomeBlame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		configure          func(*testing.T) string
		wantStateHomeEmpty bool
	}{
		{
			name: "nonexistent project directory",
			configure: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			wantStateHomeEmpty: true,
		},
		{
			name: "dangling project symlink",
			configure: func(t *testing.T) string {
				parent := t.TempDir()
				link := filepath.Join(parent, "dangling")
				if err := os.Symlink(filepath.Join(parent, "nowhere"), link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			wantStateHomeEmpty: true,
		},
		{
			name: "project path is a regular file",
			configure: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(path, []byte("not a project\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantStateHomeEmpty: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := test.configure(t)
			store := freshness.Store{BaseDirectory: t.TempDir()}
			application := &Application{
				runner:   NewRunner(store, func() time.Time { return runnerNow }, dependency.NewService(dependency.NewResolver(offlineGitHub{}))),
				fallback: cli.UnavailableApplication{},
			}

			stdout, stderr, exitCode := runFreshnessCLI(t, application, project, "outdated", true)

			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", exitCode, cli.ExitOperational, stdout, stderr)
			}
			if strings.Count(strings.TrimSpace(stdout), "\n") != 0 || strings.TrimSpace(stdout) == "" {
				t.Fatalf("stdout = %q, want exactly one JSON line", stdout)
			}
			var envelope struct {
				OK      bool   `json:"ok"`
				Command string `json:"command"`
				Result  struct {
					Notices []cli.Notice `json:"notices"`
				} `json:"result"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("stdout is not a JSON envelope %q: %v; stderr=%q", stdout, err, stderr)
			}
			if envelope.Error != nil {
				t.Fatalf("JSON error envelope on stdout = %#v, want a result envelope; stderr=%q", envelope.Error, stderr)
			}
			if envelope.OK || envelope.Command != "freshness" || len(envelope.Result.Notices) != 1 {
				t.Fatalf("envelope = %#v, want one fail-open freshness notice", envelope)
			}
			notice := envelope.Result.Notices[0]
			if notice.Code != CodeUpdateFailed {
				t.Fatalf("notice = %#v, want %s", notice, CodeUpdateFailed)
			}
			if !strings.Contains(notice.Message, project) {
				t.Fatalf("notice message %q, want the rejected path %q", notice.Message, project)
			}
			if !strings.Contains(notice.Message, "--project") {
				t.Fatalf("notice message %q, want --project guidance", notice.Message)
			}
			combined := notice.Code + notice.Message + stderr
			if strings.Contains(combined, CodeStateUnwritable) || strings.Contains(combined, "ACR_STATE_HOME") {
				t.Fatalf("blamed the state home for an unusable project root: %q", combined)
			}
			if !strings.Contains(stderr, notice.Code+": ") || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
				t.Fatalf("stderr = %q, want exactly one %s notice line", stderr, notice.Code)
			}

			entries, err := os.ReadDir(store.BaseDirectory)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantStateHomeEmpty {
				if len(entries) != 0 {
					t.Fatalf("state home entries = %v, want empty (no project identity, so no throttle write)", entries)
				}
				return
			}
			if len(entries) == 0 {
				t.Fatal("state home empty, want the canonicalizable root's failed attempt recorded")
			}
			state, usable, err := store.Read(project)
			if err != nil || !usable || state.LastCheckedAt != runnerNow || state.LastOutcome != freshness.OutcomeFailed {
				t.Fatalf("recorded state = %#v, usable = %t, error = %v; want a recorded failed attempt", state, usable, err)
			}
		})
	}
}

// TestReverifyPolicyNoneCarriesLoadFailureAndStaysSilentWhenHealthy pins both
// halves of the --policy none contract: an unreadable agents.yaml still surfaces
// one notice at exit 0, and a healthy project stays silent.
func TestReverifyPolicyNoneCarriesLoadFailureAndStaysSilentWhenHealthy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projectFile string
		wantNotices int
	}{
		{
			name:        "malformed agents.yaml",
			projectFile: "schemaVersion: [\n",
			wantNotices: 1,
		},
		{
			name:        "healthy agents.yaml",
			projectFile: "schemaVersion: 1\nfreshness: outdated\n",
			wantNotices: 0,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			writeProjectFile(t, project, dependency.ProjectFilename, test.projectFile)
			store := freshness.Store{BaseDirectory: t.TempDir()}
			application := &Application{
				runner:   NewRunner(store, func() time.Time { return runnerNow }, dependency.NewService(dependency.NewResolver(offlineGitHub{}))),
				fallback: cli.UnavailableApplication{},
			}

			stdout, stderr, exitCode := runFreshnessCLI(t, application, project, "none", true)

			if exitCode != cli.ExitSuccess {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", exitCode, cli.ExitSuccess, stdout, stderr)
			}
			if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Fatalf("stdout = %q, want exactly one JSON line", stdout)
			}
			var envelope struct {
				OK     bool `json:"ok"`
				Result struct {
					Policy  freshness.Policy `json:"policy"`
					Notices []cli.Notice     `json:"notices"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("stdout is not a JSON envelope %q: %v; stderr=%q", stdout, err, stderr)
			}
			if !envelope.OK || envelope.Result.Policy != freshness.PolicyNone {
				t.Fatalf("envelope = %#v, want ok policy none", envelope)
			}
			if len(envelope.Result.Notices) != test.wantNotices {
				t.Fatalf("notices = %#v, want %d", envelope.Result.Notices, test.wantNotices)
			}
			if test.wantNotices == 0 {
				if stderr != "" {
					t.Fatalf("stderr = %q, want silence on a healthy project", stderr)
				}
				return
			}
			if envelope.Result.Notices[0].Code != CodeUpdateFailed {
				t.Fatalf("notice = %#v, want %s", envelope.Result.Notices[0], CodeUpdateFailed)
			}
			if !strings.Contains(stderr, CodeUpdateFailed+": ") || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
				t.Fatalf("stderr = %q, want exactly one %s notice line", stderr, CodeUpdateFailed)
			}
			if strings.Contains(stderr, "ACR_STATE_HOME") {
				t.Fatalf("stderr = %q, want no state-home blame for a project-state failure", stderr)
			}
		})
	}
}
