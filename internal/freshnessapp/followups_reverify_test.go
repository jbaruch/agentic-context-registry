package freshnessapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

// Unusable project roots fail at CLI entry before freshness can record state.
func TestReverifyUnusableProjectRootStaysOneEnvelopeWithoutStateHomeBlame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*testing.T) string
		reason    string
	}{
		{
			name: "nonexistent project directory",
			configure: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			reason: "no such file or directory",
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
			reason: "no such file or directory",
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
			reason: "not a directory",
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
			message := fmt.Sprintf("validate project directory in project %q; restore directory access or verify --project, then retry: open %s/.: %s", project, project, test.reason)
			quoted, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("{\"ok\":false,\"command\":\"freshness\",\"error\":{\"code\":\"project_access_failed\",\"message\":%s}}\n", quoted)
			if stdout != "" || stderr != want {
				t.Fatalf("stdout=%q stderr=%q; want empty stdout and stderr=%q", stdout, stderr, want)
			}
			entries, err := os.ReadDir(store.BaseDirectory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("project refusal changed freshness state: %v, %v", entries, err)
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
