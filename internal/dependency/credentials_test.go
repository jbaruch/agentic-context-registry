package dependency

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// probeTraceVariable names the file each scratch command appends its own name
// to. The trace is what establishes which probes ran, in what order, and that
// a lower-priority probe was never reached.
const probeTraceVariable = "ACR_CREDENTIAL_PROBE_TRACE"

// withCredentialProbeTimeout decides the bound a credential probe runs under
// for one test and restores the shipped default afterwards. The behaviour
// cases run unbounded, so no assertion about which command answers depends on
// a forked shell winning a wall-clock deadline under whatever load the rest of
// the suite is applying; the expiration cases run with a deadline that is
// already past, so the failure mode is reached without waiting for one. Every
// test that calls this runs sequentially, because credential discovery also
// reads process-wide environment through t.Setenv.
func withCredentialProbeTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := credentialProbeTimeout
	credentialProbeTimeout = timeout
	t.Cleanup(func() { credentialProbeTimeout = previous })
}

// fakeCommandShell is the interpreter the scratch commands run under. The
// scratch PATH deliberately holds nothing but the fakes, so the shebang names a
// resolved absolute path rather than relying on a lookup that would fail.
func fakeCommandShell(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("resolve bash for the scratch commands: %v", err)
	}
	return shell
}

// writeFakeCommand puts an executable on a scratch PATH. Credential discovery
// shells out, and an injected token provider would prove nothing about the
// order those commands are actually tried in.
//
// Each fake runs under strict mode, so a fake that cannot do what it was
// written to do exits non-zero instead of continuing to the answer the test
// expects.
func writeFakeCommand(t *testing.T, shell, directory, name, script string) {
	t.Helper()
	contents := "#!" + shell + "\nset -euo pipefail\n" + script
	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ghProbeContract is the prologue in front of every gh answer. Discovery is
// only allowed to ask gh for a token, so any other argv exits non-zero and the
// case fails rather than being answered by a command that was asked the wrong
// question.
const ghProbeContract = `[ "$#" = 2 ] && [ "$1" = auth ] && [ "$2" = token ] || {
	printf 'gh received argv: %s\n' "$*" >&2
	exit 41
}
printf 'gh\n' >> "${ACR_CREDENTIAL_PROBE_TRACE}"
`

// gitProbeContract is the prologue in front of every git answer. It pins the
// subcommand, the exact bytes of the credential request including its
// terminating blank line, and the non-interactive override, all read with
// shell builtins because nothing else is on the scratch PATH. Each check
// answers a different way the production probe could go wrong, so a fake that
// exits here names which one.
const gitProbeContract = `[ "$#" = 2 ] && [ "$1" = credential ] && [ "$2" = fill ] || {
	printf 'git received argv: %s\n' "$*" >&2
	exit 42
}
printf 'git\n' >> "${ACR_CREDENTIAL_PROBE_TRACE}"
request=""
while IFS= read -r line; do
	request="${request}${line};"
done
[ "${request}" = 'protocol=https;host=github.com;;' ] || {
	printf 'git received credential request: %s\n' "${request}" >&2
	exit 43
}
[ "${GIT_TERMINAL_PROMPT:-unset}" = 0 ] || {
	printf 'git ran with GIT_TERMINAL_PROMPT=%s\n' "${GIT_TERMINAL_PROMPT:-unset}" >&2
	exit 44
}
`

// The answers both fakes give when they are meant to succeed. Each is padded,
// so discovery that stopped trimming its command output fails the case instead
// of passing it invisibly.
const (
	ghAnswers  = "printf '  from-gh-cli \\n'\n"
	gitAnswers = "printf 'username=fixture\\npassword=  from-git  \\n\\n'\n"
)

// credentialFixture builds a scratch PATH holding only the named fakes and
// returns the trace file they append to. GIT_TERMINAL_PROMPT is inherited as
// 1, so production's own override is what makes the git fake answer; dropping
// it turns every git case red instead of passing on a parent that already had
// prompting disabled. BASH_ENV is cleared so a developer's startup file cannot
// run inside a fake.
func credentialFixture(t *testing.T, shell, gh, git string) string {
	t.Helper()
	directory := t.TempDir()
	trace := filepath.Join(directory, "trace")
	t.Setenv(probeTraceVariable, trace)
	t.Setenv("PATH", directory)
	t.Setenv("BASH_ENV", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if gh != "" {
		writeFakeCommand(t, shell, directory, "gh", ghProbeContract+gh)
	}
	if git != "" {
		writeFakeCommand(t, shell, directory, "git", gitProbeContract+git)
	}
	return trace
}

// assertProbeTrace holds discovery to the probes it was supposed to run, in
// order. An absent file and an empty expectation are the same outcome: no
// command was started.
func assertProbeTrace(t *testing.T, trace, want string) {
	t.Helper()
	contents, err := os.ReadFile(trace)
	if errors.Is(err, os.ErrNotExist) {
		contents = nil
	} else if err != nil {
		t.Fatalf("read the probe trace: %v", err)
	}
	if string(contents) != want {
		t.Fatalf("probe trace = %q, want %q", contents, want)
	}
}

func TestDiscoverGitHubTokenPrefersEnvironmentThenCommands(t *testing.T) {
	shell := fakeCommandShell(t)
	// Unbounded: the outcome of every case below is decided by which command
	// discovery runs and what it answers, never by whether the shell that
	// answers started before a production deadline expired.
	withCredentialProbeTimeout(t, 0)
	for _, test := range []struct {
		name         string
		ghToken      string
		githubToken  string
		gh           string
		git          string
		unexecutable bool
		want         string
		trace        string
	}{
		{name: "GH_TOKEN wins before any command runs", ghToken: "  from-gh-token \t", githubToken: "from-github-token", gh: ghAnswers, git: gitAnswers, want: "from-gh-token"},
		{name: "a blank GH_TOKEN is not an answer", ghToken: " \n", githubToken: "  from-github-token  ", gh: ghAnswers, git: gitAnswers, want: "from-github-token"},
		{name: "gh follows the environment", gh: ghAnswers, git: gitAnswers, want: "from-gh-cli", trace: "gh\n"},
		{name: "a failing gh has its output ignored", gh: "printf 'unreachable\\n'\nexit 7\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "an empty gh answer falls through", gh: "printf '  \\n'\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "a missing gh falls through", git: gitAnswers, want: "from-git", trace: "git\n"},
		{name: "an unexecutable gh falls through", gh: ghAnswers, git: gitAnswers, unexecutable: true, want: "from-git", trace: "git\n"},
		{name: "git credential fill is the last resort", gh: "exit 1\n", git: gitAnswers, want: "from-git", trace: "gh\ngit\n"},
		{name: "a failing git answer is not a credential", gh: "exit 1\n", git: "printf 'password=unreachable\\n'\nexit 8\n", want: "", trace: "gh\ngit\n"},
		{name: "an empty git password is not a credential", gh: "exit 1\n", git: "printf 'password=  \\n'\n", want: "", trace: "gh\ngit\n"},
		{name: "a git answer carrying no password is not a credential", gh: "exit 1\n", git: "printf 'username=fixture\\n'\n", want: "", trace: "gh\ngit\n"},
		{name: "no credential is a public client", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := credentialFixture(t, shell, test.gh, test.git)
			t.Setenv("GH_TOKEN", test.ghToken)
			t.Setenv("GITHUB_TOKEN", test.githubToken)
			if test.unexecutable {
				if err := os.Chmod(filepath.Join(filepath.Dir(trace), "gh"), 0o600); err != nil {
					t.Fatalf("make the gh fake unexecutable: %v", err)
				}
			}

			if got := discoverGitHubToken(context.Background()); got != test.want {
				t.Fatalf("discoverGitHubToken() = %q, want %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)
		})
	}
}

// TestCredentialProbeContextBoundsEveryProductionProbe pins the deadline the
// shipped binary puts on one credential probe. The instant it is measured from
// is supplied, and the five seconds it expects is written here rather than read
// back out of the production constant, so the assertion is an independent
// oracle and needs no clock to run down.
func TestCredentialProbeContextBoundsEveryProductionProbe(t *testing.T) {
	const wantProductionTimeout = 5 * time.Second
	if credentialProbeTimeout != wantProductionTimeout {
		t.Fatalf("production probe timeout = %v, want %v", credentialProbeTimeout, wantProductionTimeout)
	}
	// A fixed instant, far enough ahead that no deadline derived from it has
	// passed, and far enough from the real clock that a stall cannot supply the
	// answer instead of the arithmetic.
	frozen := time.Date(2222, time.February, 2, 2, 2, 2, 0, time.UTC)
	bounded, cancelBounded := credentialProbeContext(context.Background(), frozen)
	defer cancelBounded()

	deadline, ok := bounded.Deadline()
	if !ok {
		t.Fatal("the production probe context carries no deadline; a hung credential helper would stall resolution")
	}
	if !deadline.Equal(frozen.Add(wantProductionTimeout)) {
		t.Fatalf("probe deadline = %v, want %v after the supplied instant", deadline, wantProductionTimeout)
	}

	// The same construction with an instant already behind us is expired on
	// arrival, which is how the expiration cases below reach the failure mode
	// without waiting for a deadline.
	past := time.Date(1999, time.September, 9, 9, 9, 9, 0, time.UTC)
	expired, cancelExpired := credentialProbeContext(context.Background(), past)
	defer cancelExpired()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("probe context from a past instant error = %v, want an exceeded deadline", expired.Err())
	}

	// The test-only unbounded setting installs no deadline at all, which is
	// what keeps the behaviour fixtures above from racing anything.
	withCredentialProbeTimeout(t, 0)
	unbounded, cancelUnbounded := credentialProbeContext(context.Background(), frozen)
	defer cancelUnbounded()
	if _, ok := unbounded.Deadline(); ok {
		t.Fatal("a zero probe timeout installed a deadline; the behaviour fixtures would still race it")
	}
}

// TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded covers the
// failure mode that made the precedence suite flaky: a probe whose deadline is
// gone starts no command and contributes nothing, and discovery moves on. The
// controls run the same fakes unbounded first, so the expired run is compared
// against commands that demonstrably answer rather than against nothing.
func TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded(t *testing.T) {
	shell := fakeCommandShell(t)
	trace := credentialFixture(t, shell, ghAnswers, gitAnswers)

	withCredentialProbeTimeout(t, 0)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "from-gh-cli" {
		t.Fatalf("control commandToken(gh) = %q, want the fake's answer", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "from-gh-cli" {
		t.Fatalf("control discoverGitHubToken() = %q, want the fake's answer", got)
	}
	assertProbeTrace(t, trace, "gh\ngh\n")

	expiredTrace := credentialFixture(t, shell, ghAnswers, gitAnswers)
	withCredentialProbeTimeout(t, -time.Second)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "" {
		t.Fatalf("expired commandToken(gh) = %q, want no token", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "" {
		t.Fatalf("expired discoverGitHubToken() = %q, want the anonymous client", got)
	}
	assertProbeTrace(t, expiredTrace, "")
}

// TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled keeps discovery
// from answering out of a dead context under the production bound, and from
// starting a command it cannot use. The environment sources still answer,
// because they read no process.
func TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled(t *testing.T) {
	shell := fakeCommandShell(t)
	trace := credentialFixture(t, shell, ghAnswers, gitAnswers)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := discoverGitHubToken(cancelled); got != "" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want no token", got)
	}
	assertProbeTrace(t, trace, "")

	t.Setenv("GH_TOKEN", "  from-gh-token  ")
	if got := discoverGitHubToken(cancelled); got != "from-gh-token" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want the environment token", got)
	}
	assertProbeTrace(t, trace, "")
}

// TestGitHubClientCachesTheFirstDiscoveryThroughTheRealProbes runs the shipped
// client over the shipped discovery path and then changes every credential it
// could read. A later request has to carry the first answer and start no
// second probe, so removing the cache is a failing assertion rather than two
// identical headers from a provider that answers the same way twice. The
// anonymous case is the one that fails silently everywhere else: an empty
// header looks the same whether it was cached or rediscovered.
func TestGitHubClientCachesTheFirstDiscoveryThroughTheRealProbes(t *testing.T) {
	shell := fakeCommandShell(t)
	for _, test := range []struct {
		name  string
		gh    string
		git   string
		want  string
		trace string
	}{
		{name: "an authenticated first answer", gh: ghAnswers, git: gitAnswers, want: "Bearer from-gh-cli", trace: "gh\n"},
		{name: "an anonymous first answer", gh: "exit 1\n", git: "exit 1\n", want: "", trace: "gh\ngit\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := credentialFixture(t, shell, test.gh, test.git)
			withCredentialProbeTimeout(t, 0)
			client := NewGitHubClient()

			// The first request is cancelled: discovery must still run, and the
			// request must still carry its own cancellation.
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			first, err := client.newRequest(cancelled, http.MethodGet, "https://example.invalid/first", nil)
			if err != nil {
				t.Fatalf("first newRequest() error = %v", err)
			}
			if !errors.Is(first.Context().Err(), context.Canceled) {
				t.Fatal("the first request lost its own cancellation")
			}
			if got := first.Header.Get("Authorization"); got != test.want {
				t.Fatalf("first Authorization = %q, want %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)

			// Every source a second discovery could read now answers
			// differently. Nothing the client sends may change.
			t.Setenv("GH_TOKEN", "rediscovered")
			writeFakeCommand(t, shell, filepath.Dir(trace), "gh", ghProbeContract+"printf 'rediscovered\\n'\n")
			second, err := client.newRequest(context.Background(), http.MethodGet, "https://example.invalid/second", nil)
			if err != nil {
				t.Fatalf("second newRequest() error = %v", err)
			}
			if second.Context().Err() != nil {
				t.Fatalf("the second request inherited cancellation: %v", second.Context().Err())
			}
			if got := second.Header.Get("Authorization"); got != test.want {
				t.Fatalf("second Authorization = %q, want the cached %q", got, test.want)
			}
			assertProbeTrace(t, trace, test.trace)
		})
	}
}
