package dependency

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
printf 'password=from-git\n'
`

func TestDiscoverGitHubTokenPrefersEnvironmentThenCommands(t *testing.T) {
	shell := fakeCommandShell(t)
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
		{name: "gh follows the environment", gh: "printf 'from-gh-cli\\n'\n", git: "printf 'password=unreachable\\n'\n", want: "from-gh-cli"},
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

			// Budget 0: this test establishes the order the sources are
			// tried in, and nothing about that order depends on how long a
			// spawn takes. Racing the production budget is what made this
			// assertion report a wrong order under a loaded full suite; the
			// budget itself is covered below, on a clock the test owns.
			if got := discoverGitHubTokenWithin(context.Background(), 0); got != test.want {
				t.Fatalf("discoverGitHubTokenWithin() = %q, want %q", got, test.want)
			}
		})
	}
}

// blockingCredentialFake never answers. It reads its request so a discovery
// that sends one still gets consumed, then waits to be killed.
const blockingCredentialFake = `while IFS= read -r line; do
	:
done
while :; do
	sleep 1
done
`

// TestCommandTokenAbandonsABlockedCredentialCommand covers the budget the
// order test above deliberately switches off: a credential helper that never
// answers is abandoned, discovery returns no token, and the caller is not
// held.
//
// A budget test can only be written this way round. Asserting that a command
// finishes inside a budget is the wall-clock coupling this whole change
// removes — a loaded machine misses any budget a fast one meets. Asserting
// that a command which never answers is abandoned holds on every machine,
// because a slower one only overshoots the deadline further. Together with
// the order table above, which reaches the second source with no deadline at
// all, that establishes both halves of the wiring without a race.
func TestCommandTokenAbandonsABlockedCredentialCommand(t *testing.T) {
	shell := fakeCommandShell(t)
	directory := t.TempDir()
	writeFakeCommand(t, shell, directory, "git", blockingCredentialFake)
	t.Setenv("PATH", directory)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	input := []byte("protocol=https\nhost=github.com\n\n")
	if got := commandToken(context.Background(), 50*time.Millisecond, "git", []string{"credential", "fill"}, input); got != "" {
		t.Fatalf("commandToken() = %q, want no token from a command that never answered", got)
	}
}

// TestDiscoverGitHubTokenShipsTheProductionBudget binds the exported entry
// point to the budget the binary runs with, so making the budget injectable
// cannot quietly leave production without one.
func TestDiscoverGitHubTokenShipsTheProductionBudget(t *testing.T) {
	if commandTokenBudget <= 0 {
		t.Fatalf("commandTokenBudget = %v, want a positive per-command deadline", commandTokenBudget)
	}
}
