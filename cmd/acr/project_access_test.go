package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the shipped entry point before setup, recovery, Git inspection or
// freshness throttling can alter the error or write state.
func TestProjectAccessExactOutput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("project permission matrix requires a non-root test process")
	}
	binary := journeyBuiltBinary(t)
	packagePath, err := filepath.Abs("../../examples/complete")
	if err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"init"}, {"install"}, {"realize"}, {"list"}, {"outdated"},
		{"freshness", "run"}, {"update"}, {"resume", "github:owner/plugin"},
		{"uninstall", "github:owner/plugin"}, {"check"}, {"validate"}, {"publish"},
		{"migrate", "tessl"}, {"migrate", "tessl-plugin"},
		{"install", "github:owner/plugin"}, {"install", "file:" + packagePath},
		{"validate", packagePath}, {"publish", packagePath},
		{"migrate", "tessl-plugin", packagePath},
		{"migrate", "tessl-plugin", packagePath, "--acr-only", "--repository", "https://github.com/owner/plugin"},
	}
	for _, argv := range commands {
		for _, kind := range []string{"missing", "regular-file", "mode-000"} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/json=%t", strings.Join(argv, " "), kind, jsonOutput), func(t *testing.T) {
					project := filepath.Join(t.TempDir(), "project with spaces")
					reason := "no such file or directory"
					switch kind {
					case "regular-file":
						if err := os.WriteFile(project, []byte("not a directory\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						reason = "not a directory"
					case "mode-000":
						if err := os.Mkdir(project, 0o700); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := os.Chmod(project, 0o700); err != nil {
								t.Error(err)
							}
						})
						if err := os.Chmod(project, 0); err != nil {
							t.Fatal(err)
						}
						reason = "permission denied"
					}
					args := append(append([]string{}, argv...), "--project", project)
					message := fmt.Sprintf("validate project directory in project %q; restore directory access or verify --project, then retry: open %s/.: %s", project, project, reason)
					want := "acr " + argv[0] + ": " + message + "\n"
					if jsonOutput {
						args = append(args, "--json")
						quoted, err := json.Marshal(message)
						if err != nil {
							t.Fatal(err)
						}
						want = fmt.Sprintf("{\"ok\":false,\"command\":%q,\"error\":{\"code\":\"project_access_failed\",\"message\":%s}}\n", argv[0], quoted)
					}
					// runACR uses piped stdout/stderr and a nil (closed) stdin.
					state := t.TempDir()
					stdout, stderr, code := runACR(t, binary, args, environmentWith("ACR_STATE_HOME", state))
					if code != 1 || stdout != "" || stderr != want {
						t.Fatalf("exit=%d stdout=%q stderr=%q; want exit=1 stdout empty stderr=%q", code, stdout, stderr, want)
					}
					entries, err := os.ReadDir(state)
					if err != nil || len(entries) != 0 {
						t.Fatalf("project refusal changed independent state: %v, %v", entries, err)
					}
				})
			}
		}
	}
}
