package dependency

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// withCredentialProbeTimeout decides the bound a credential probe runs under
// for one test and restores the shipped default afterwards. The precedence
// cases run unbounded, so no assertion about which command answers depends on
// a forked shell winning a wall-clock deadline under whatever load the rest of
// the suite is applying; the timeout cases run with a deadline that is already
// past, so the failure mode is reached without waiting for one. Every test
// that calls this runs sequentially, because credential discovery also reads
// process-wide environment through t.Setenv.
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

// gitCredentialFake answers a credential request only after reading one that
// names the host, using shell builtins alone. An external reader would not be
// on the scratch PATH, and a fake that answered without reading would prove
// nothing about what discovery sends.
const gitCredentialFake = `request=""
while IFS= read -r line; do
	request="${request}${line};"
done
case "${request}" in
*host=github.com*) ;;
*) printf 'the credential request named no host: %s\n' "${request}" >&2; exit 1 ;;
esac
# The fake refuses to answer unless prompting is disabled, so the contract that
# keeps a credential helper from blocking is proven rather than assumed.
[ "${GIT_TERMINAL_PROMPT:-}" = 0 ] || exit 1
printf 'password=  from-git  \n'
`

// ghAnswers pads the token it prints so a discovery that stopped trimming its
// command output fails the precedence assertions instead of passing them.
const ghAnswers = "printf '  from-gh-cli \\n'\n"

func TestDiscoverGitHubTokenPrefersEnvironmentThenCommands(t *testing.T) {
	shell := fakeCommandShell(t)
	// Unbounded: the outcome of every case below is decided by which command
	// discovery runs and what it answers, never by whether the shell that
	// answers started before a production deadline expired.
	withCredentialProbeTimeout(t, 0)
	for _, test := range []struct {
		name        string
		ghToken     string
		githubToken string
		gh          string
		git         string
		want        string
	}{
		{name: "GH_TOKEN wins", ghToken: "from-gh-token", githubToken: "from-github-token", want: "from-gh-token"},
		{name: "GITHUB_TOKEN is next", githubToken: "from-github-token", gh: "printf 'unreachable\\n'\n", want: "from-github-token"},
		{name: "gh follows the environment", gh: ghAnswers, git: "printf 'password=unreachable\\n'\n", want: "from-gh-cli"},
		{name: "git credential fill is the last resort", gh: "exit 1\n", git: gitCredentialFake, want: "from-git"},
		{name: "no credential is a public client", gh: "exit 1\n", git: "exit 1\n", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if test.gh != "" {
				writeFakeCommand(t, shell, directory, "gh", test.gh)
			}
			if test.git != "" {
				writeFakeCommand(t, shell, directory, "git", test.git)
			}
			t.Setenv("PATH", directory)
			t.Setenv("GH_TOKEN", test.ghToken)
			t.Setenv("GITHUB_TOKEN", test.githubToken)

			if got := discoverGitHubToken(context.Background()); got != test.want {
				t.Fatalf("discoverGitHubToken() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestCredentialProbeContextBoundsEveryProductionProbe pins the deadline the
// shipped binary puts on one credential probe. It reads the deadline the
// production setting installs rather than timing a command, so the assertion
// says the same thing on an idle machine and a loaded one.
func TestCredentialProbeContextBoundsEveryProductionProbe(t *testing.T) {
	if credentialProbeTimeout != defaultCredentialProbeTimeout {
		t.Fatalf("credentialProbeTimeout = %v, want the shipped %v", credentialProbeTimeout, defaultCredentialProbeTimeout)
	}
	before := time.Now()
	bounded, cancelBounded := credentialProbeContext(context.Background())
	after := time.Now()
	defer cancelBounded()

	deadline, ok := bounded.Deadline()
	if !ok {
		t.Fatal("the production probe context carries no deadline; a hung credential helper would stall resolution")
	}
	if deadline.Before(before.Add(defaultCredentialProbeTimeout)) || deadline.After(after.Add(defaultCredentialProbeTimeout)) {
		t.Fatalf("probe deadline = %v, want %v after the call", deadline, defaultCredentialProbeTimeout)
	}
	if err := bounded.Err(); err != nil {
		t.Fatalf("production probe context error = %v, want a live context", err)
	}

	// The seam's two test-only settings, stated as behaviour so a later change
	// to credentialProbeContext cannot quietly retune the fixtures below.
	withCredentialProbeTimeout(t, 0)
	unbounded, cancelUnbounded := credentialProbeContext(context.Background())
	defer cancelUnbounded()
	if _, ok := unbounded.Deadline(); ok {
		t.Fatal("a zero probe timeout installed a deadline; the precedence fixtures would still race it")
	}

	withCredentialProbeTimeout(t, -time.Second)
	expired, cancelExpired := credentialProbeContext(context.Background())
	defer cancelExpired()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("negative probe timeout error = %v, want an already-exceeded deadline", expired.Err())
	}
}

// TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded covers the
// failure mode that made the precedence suite flaky: a probe whose deadline is
// gone contributes nothing, and discovery moves on. The controls run the same
// fakes unbounded first, so the expired run is compared against commands that
// demonstrably answer rather than against nothing.
func TestDiscoverGitHubTokenFallsBackWhenEveryProbeDeadlineIsExceeded(t *testing.T) {
	shell := fakeCommandShell(t)
	directory := t.TempDir()
	writeFakeCommand(t, shell, directory, "gh", ghAnswers)
	writeFakeCommand(t, shell, directory, "git", gitCredentialFake)
	t.Setenv("PATH", directory)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	withCredentialProbeTimeout(t, 0)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "from-gh-cli" {
		t.Fatalf("control commandToken(gh) = %q, want the fake's answer", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "from-gh-cli" {
		t.Fatalf("control discoverGitHubToken() = %q, want the fake's answer", got)
	}

	withCredentialProbeTimeout(t, -time.Second)
	if got := commandToken(context.Background(), "gh", []string{"auth", "token"}, nil); got != "" {
		t.Fatalf("expired commandToken(gh) = %q, want no token", got)
	}
	if got := discoverGitHubToken(context.Background()); got != "" {
		t.Fatalf("expired discoverGitHubToken() = %q, want the anonymous client", got)
	}
}

// TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled keeps discovery
// from answering out of a dead context under the production bound. The
// environment sources still answer, because they read no process.
func TestDiscoverGitHubTokenReturnsNoTokenWhenTheCallerIsCancelled(t *testing.T) {
	shell := fakeCommandShell(t)
	directory := t.TempDir()
	writeFakeCommand(t, shell, directory, "gh", ghAnswers)
	writeFakeCommand(t, shell, directory, "git", gitCredentialFake)
	t.Setenv("PATH", directory)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := discoverGitHubToken(cancelled); got != "" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want no token", got)
	}

	t.Setenv("GH_TOKEN", "  from-gh-token  ")
	if got := discoverGitHubToken(cancelled); got != "from-gh-token" {
		t.Fatalf("discoverGitHubToken(cancelled) = %q, want the environment token", got)
	}
}
