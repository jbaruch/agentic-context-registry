package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunnerParsesCommandContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want Invocation
	}{
		{
			name: "init",
			args: []string{"init", "--project", "/project", "--agent", "codex", "--agent=claude-code", "--freshness", "install", "--non-interactive", "--dry-run", "--json"},
			want: Invocation{
				Command:           CommandInit,
				ProjectDirectory:  "/project",
				Output:            OutputJSON,
				DryRun:            true,
				NonInteractive:    true,
				Agents:            []string{"codex", "claude-code"},
				Freshness:         FreshnessInstall,
				FreshnessExplicit: true,
			},
		},
		{
			name: "install latest",
			args: []string{"install", "github:owner/plugin"},
			want: Invocation{
				Command:          CommandInstall,
				ProjectDirectory: ".",
				Output:           OutputText,
				Freshness:        FreshnessOutdated,
				Source:           "github:owner/plugin",
				RequestedVersion: "latest",
			},
		},
		{
			name: "install pinned",
			args: []string{"install", "github:owner/plugin@v1.2.3", "--freshness=none"},
			want: Invocation{
				Command:           CommandInstall,
				ProjectDirectory:  ".",
				Output:            OutputText,
				Freshness:         FreshnessNone,
				FreshnessExplicit: true,
				Source:            "github:owner/plugin",
				RequestedVersion:  "v1.2.3",
			},
		},
		{
			name: "install reconcile",
			args: []string{"install", "--non-interactive"},
			want: Invocation{
				Command:          CommandInstall,
				ProjectDirectory: ".",
				Output:           OutputText,
				NonInteractive:   true,
				Freshness:        FreshnessOutdated,
				Reconcile:        true,
			},
		},
		{
			name: "realize",
			args: []string{"realize", "--dry-run"},
			want: Invocation{Command: CommandRealize, ProjectDirectory: ".", Output: OutputText, DryRun: true},
		},
		{
			name: "list",
			args: []string{"list", "--json"},
			want: Invocation{Command: CommandList, ProjectDirectory: ".", Output: OutputJSON},
		},
		{
			name: "outdated",
			args: []string{"outdated"},
			want: Invocation{Command: CommandOutdated, ProjectDirectory: ".", Output: OutputText},
		},
		{
			name: "update one",
			args: []string{"update", "github:owner/plugin", "--dry-run"},
			want: Invocation{Command: CommandUpdate, ProjectDirectory: ".", Output: OutputText, DryRun: true, Source: "github:owner/plugin"},
		},
		{
			name: "uninstall",
			args: []string{"uninstall", "github:owner/plugin"},
			want: Invocation{Command: CommandUninstall, ProjectDirectory: ".", Output: OutputText, Source: "github:owner/plugin"},
		},
		{
			name: "check",
			args: []string{"check", "--project=/project"},
			want: Invocation{Command: CommandCheck, ProjectDirectory: "/project", Output: OutputText},
		},
		{
			name: "publish current directory",
			args: []string{"publish"},
			want: Invocation{Command: CommandPublish, ProjectDirectory: ".", Output: OutputText, PublicationPath: "."},
		},
		{
			name: "migrate tessl",
			args: []string{"migrate", "tessl", "--dry-run", "--non-interactive"},
			want: Invocation{Command: CommandMigrate, Subcommand: "tessl", ProjectDirectory: ".", Output: OutputText, DryRun: true, NonInteractive: true},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got Invocation
			app := ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
				got = invocation
				return Result{Message: "ok", Value: map[string]bool{"accepted": true}}, nil
			})
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), test.args)

			if exitCode != ExitSuccess {
				t.Fatalf("Run(%v) exit code = %d, want %d; stderr = %q", test.args, exitCode, ExitSuccess, stderr.String())
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Run(%v) invocation = %#v, want %#v", test.args, got, test.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("Run(%v) stderr = %q, want empty", test.args, stderr.String())
			}
		})
	}
}

func TestRunnerHelpListsEveryCommand(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	runner := New(&stdout, &stderr, rejectingApplication(t), "test")

	if exitCode := runner.Run(context.Background(), nil); exitCode != ExitSuccess {
		t.Fatalf("Run() exit code = %d, want %d", exitCode, ExitSuccess)
	}
	for _, command := range commandOrder {
		if !strings.Contains(stdout.String(), string(command)) {
			t.Errorf("root help does not contain %q", command)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunnerCommandHelpDoesNotDispatch(t *testing.T) {
	t.Parallel()

	for _, command := range commandOrder {
		command := command
		t.Run(string(command), func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runner := New(&stdout, &stderr, rejectingApplication(t), "test")

			exitCode := runner.Run(context.Background(), []string{string(command), "--help"})

			if exitCode != ExitSuccess {
				t.Fatalf("Run(%s --help) exit code = %d, want %d", command, exitCode, ExitSuccess)
			}
			if !strings.Contains(stdout.String(), commandSpecs[command].usage) {
				t.Fatalf("Run(%s --help) stdout = %q, want usage %q", command, stdout.String(), commandSpecs[command].usage)
			}
			if stderr.Len() != 0 {
				t.Fatalf("Run(%s --help) stderr = %q, want empty", command, stderr.String())
			}
		})
	}
}

func TestRunnerJSONOutputContract(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
			return Result{Value: map[string]int{"dependencies": 2}}, nil
		})
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), []string{"list", "--json"})

		if exitCode != ExitSuccess {
			t.Fatalf("Run(list --json) exit code = %d, want %d", exitCode, ExitSuccess)
		}
		var envelope struct {
			OK      bool           `json:"ok"`
			Command string         `json:"command"`
			Result  map[string]int `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("decode JSON stdout %q: %v", stdout.String(), err)
		}
		if !envelope.OK || envelope.Command != "list" || envelope.Result["dependencies"] != 2 {
			t.Fatalf("JSON envelope = %#v, want successful list result", envelope)
		}
		if stderr.Len() != 0 {
			t.Fatalf("Run(list --json) stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("usage error", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"missing", "--json"})

		if exitCode != ExitUsage {
			t.Fatalf("Run(missing --json) exit code = %d, want %d", exitCode, ExitUsage)
		}
		if stdout.Len() != 0 {
			t.Fatalf("Run(missing --json) stdout = %q, want empty", stdout.String())
		}
		var envelope struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
			t.Fatalf("decode JSON stderr %q: %v", stderr.String(), err)
		}
		if envelope.OK || envelope.Error.Code != "usage" {
			t.Fatalf("JSON error envelope = %#v, want usage failure", envelope)
		}
	})
}

func TestRunnerPreservesApplicationExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "operational fallback", err: errors.New("network unavailable"), want: ExitOperational},
		{name: "changes required", err: &Error{ExitCode: ExitChanges, Code: "changes_required", Message: "project differs"}, want: ExitChanges},
		{name: "conflict", err: &Error{ExitCode: ExitConflict, Code: "conflict", Message: "managed content changed"}, want: ExitConflict},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
				return Result{}, test.err
			})
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), []string{"check"})

			if exitCode != test.want {
				t.Fatalf("Run(check) exit code = %d, want %d", exitCode, test.want)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("Run(check) stdout = %q, stderr = %q; want diagnostic only on stderr", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunnerRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown command", args: []string{"missing"}},
		{name: "unsupported flag", args: []string{"list", "--dry-run"}},
		{name: "missing uninstall source", args: []string{"uninstall"}},
		{name: "too many install sources", args: []string{"install", "one", "two"}},
		{name: "empty install version", args: []string{"install", "github:owner/plugin@"}},
		{name: "unsupported migration", args: []string{"migrate", "legacy"}},
		{name: "invalid freshness", args: []string{"init", "--freshness", "always"}},
		{name: "empty project", args: []string{"check", "--project="}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), test.args)

			if exitCode != ExitUsage {
				t.Fatalf("Run(%v) exit code = %d, want %d", test.args, exitCode, ExitUsage)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("Run(%v) stdout = %q, stderr = %q; want usage diagnostic only on stderr", test.args, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunnerVersionTextAndJSON(t *testing.T) {
	t.Parallel()

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "1.2.3").Run(context.Background(), []string{"version"})
		if exitCode != ExitSuccess || strings.TrimSpace(stdout.String()) != "1.2.3" || stderr.Len() != 0 {
			t.Fatalf("Run(version) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "1.2.3").Run(context.Background(), []string{"version", "--json"})
		if exitCode != ExitSuccess || !strings.Contains(stdout.String(), `"version":"1.2.3"`) || stderr.Len() != 0 {
			t.Fatalf("Run(version --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})
}

func rejectingApplication(t *testing.T) Application {
	t.Helper()
	return ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
		t.Fatalf("application unexpectedly received invocation %#v", invocation)
		return Result{}, nil
	})
}
