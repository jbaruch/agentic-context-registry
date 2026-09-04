package dependency

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeCommand puts an executable on a scratch PATH. Credential discovery
// shells out, and an injected token provider would prove nothing about the
// order those commands are actually tried in.
func writeFakeCommand(t *testing.T, directory, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverGitHubTokenPrefersEnvironmentThenCommands(t *testing.T) {
	for _, test := range []struct {
		name        string
		ghToken     string
		githubToken string
		gh          string
		git         string
		want        string
	}{
		{name: "GH_TOKEN wins", ghToken: "from-gh-token", githubToken: "from-github-token", want: "from-gh-token"},
		{name: "GITHUB_TOKEN is next", githubToken: "from-github-token", gh: "echo unreachable\n", want: "from-github-token"},
		{name: "gh follows the environment", gh: "echo from-gh-cli\n", git: "echo password=unreachable\n", want: "from-gh-cli"},
		{
			name: "git credential fill is the last resort",
			gh:   "exit 1\n",
			// The fake refuses to answer unless prompting is disabled, so the
			// contract that keeps a credential helper from blocking is proven
			// rather than assumed.
			git:  "cat >/dev/null\n[ \"$GIT_TERMINAL_PROMPT\" = 0 ] || exit 1\necho password=from-git\n",
			want: "from-git",
		},
		{name: "no credential is a public client", gh: "exit 1\n", git: "exit 1\n", want: ""},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if test.gh != "" {
				writeFakeCommand(t, directory, "gh", test.gh)
			}
			if test.git != "" {
				writeFakeCommand(t, directory, "git", test.git)
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
