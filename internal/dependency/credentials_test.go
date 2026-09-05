package dependency

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
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

// blockingCredentialFake blocks with shell builtins alone, because the
// scratch PATH holds nothing but the fakes and an external command would
// simply fail to resolve — which an empty result cannot be told apart from,
// and which is how an earlier version of this fixture passed a cancellation
// assertion without ever blocking.
//
// It consumes the credential request, records that it reached the blocked
// state, then opens a FIFO for reading. That open does not return until a
// writer appears, and nothing ever writes, so the process sits in the kernel
// until it is killed.
const blockingCredentialFake = `while IFS= read -r line; do
	:
done
: > "${CREDENTIAL_FAKE_STARTED}"
read -r answer < "${CREDENTIAL_FAKE_FIFO}"
printf 'password=unreachable\n'
`

// blockedCredentialCommand puts the blocking fake on a scratch PATH and
// returns the marker the fake writes once it is blocked, so a caller can
// require that the process really got there.
func blockedCredentialCommand(t *testing.T, name string) string {
	t.Helper()
	shell := fakeCommandShell(t)
	directory := t.TempDir()
	writeFakeCommand(t, shell, directory, name, blockingCredentialFake)

	fifo := filepath.Join(t.TempDir(), "credential.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create the blocking fixture's fifo: %v", err)
	}
	started := filepath.Join(t.TempDir(), "started")

	t.Setenv("PATH", directory)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("CREDENTIAL_FAKE_FIFO", fifo)
	t.Setenv("CREDENTIAL_FAKE_STARTED", started)
	return started
}

// assertReachedTheBlockedState fails unless the fake wrote its marker, which
// is what separates "the command was abandoned while blocked" from "the
// command never ran".
func assertReachedTheBlockedState(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the blocking fake never reached its blocked state: %v", err)
	}
}

// waitForTheBlockedState blocks until the fake records that it is blocked, so
// a cancellation test cancels a running process rather than racing its spawn.
func waitForTheBlockedState(t *testing.T, marker string) {
	t.Helper()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("inspect the blocking fixture's marker: %v", err)
		}
	}
}

// TestCommandTokenHonoursCallerCancellation proves the caller's context
// reaches the subprocess, so a cancelled install abandons a blocked credential
// helper with no deadline involved at all. The test waits for the fake to
// record that it is blocked before cancelling, so it cancels a running process
// rather than racing its spawn — no clock is involved in the outcome.
func TestCommandTokenHonoursCallerCancellation(t *testing.T) {
	marker := blockedCredentialCommand(t, "git")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitForTheBlockedState(t, marker)
		cancel()
	}()

	input := []byte("protocol=https\nhost=github.com\n\n")
	if got := commandToken(ctx, 0, "git", []string{"credential", "fill"}, input); got != "" {
		t.Fatalf("commandToken() = %q, want no token after the caller cancelled", got)
	}
	assertReachedTheBlockedState(t, marker)
}

// TestDiscoverGitHubTokenAbandonsABlockedCommand binds the production entry
// point — the one the binary calls, which takes no budget argument — to an
// observable outcome rather than to a constant being positive: a helper that
// has reached its blocked state and will never answer is abandoned, and
// discovery falls through to the public client. The context is never
// cancelled, so the per-command budget is the only thing that can have ended
// it. It waits out the real commandTokenBudget on purpose; that wait is the
// wiring under test, and the marker keeps a fixture that never ran from
// passing as one that was abandoned.
func TestDiscoverGitHubTokenAbandonsABlockedCommand(t *testing.T) {
	marker := blockedCredentialCommand(t, "git")

	if got := discoverGitHubToken(context.Background()); got != "" {
		t.Fatalf("discoverGitHubToken() = %q, want no token from a blocked helper", got)
	}
	assertReachedTheBlockedState(t, marker)
}

// TestDiscoverGitHubTokenHonoursCallerCancellation proves the production entry
// point threads its caller's context all the way to the subprocess, without
// waiting out a deadline.
func TestDiscoverGitHubTokenHonoursCallerCancellation(t *testing.T) {
	marker := blockedCredentialCommand(t, "git")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitForTheBlockedState(t, marker)
		cancel()
	}()

	if got := discoverGitHubToken(ctx); got != "" {
		t.Fatalf("discoverGitHubToken() = %q, want no token after the caller cancelled", got)
	}
	assertReachedTheBlockedState(t, marker)
}
