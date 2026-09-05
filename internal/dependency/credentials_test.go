package dependency

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

			if got := discoverGitHubToken(context.Background()); got != test.want {
				t.Fatalf("discoverGitHubToken() = %q, want %q", got, test.want)
			}
		})
	}
}
