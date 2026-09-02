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
			args: []string{"realize", "--agent", "cursor", "--agent=codex", "--dry-run"},
			want: Invocation{Command: CommandRealize, ProjectDirectory: ".", Output: OutputText, DryRun: true, Agents: []string{"cursor", "codex"}},
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
			name: "freshness",
			args: []string{"freshness", "run", "--project", "/project", "--policy", "install", "--json"},
			want: Invocation{
				Command: CommandFreshness, Subcommand: "run", ProjectDirectory: "/project", Output: OutputJSON,
				Freshness: FreshnessInstall, FreshnessExplicit: true,
			},
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
			args: []string{"check", "--project=/project", "--agent", "claude-code"},
			want: Invocation{Command: CommandCheck, ProjectDirectory: "/project", Output: OutputText, Agents: []string{"claude-code"}},
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

func TestRunnerMetaCommandHelpDoesNotDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		wantUsage string
	}{
		{name: "version", args: []string{"help", "version"}, wantUsage: "acr version [--json]"},
		{name: "help", args: []string{"help", "help"}, wantUsage: "acr help [COMMAND]"},
		{name: "help flag", args: []string{"help", "--help"}, wantUsage: "acr help [COMMAND]"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), test.args)

			if exitCode != ExitSuccess || !strings.Contains(stdout.String(), test.wantUsage) || stderr.Len() != 0 {
				t.Fatalf("Run(%v) exit = %d, stdout = %q, stderr = %q; want usage %q", test.args, exitCode, stdout.String(), stderr.String(), test.wantUsage)
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
		if strings.Contains(stderr.String(), `"command"`) {
			t.Fatalf("Run(missing --json) stderr = %q, want no empty command field", stderr.String())
		}
	})

	t.Run("flag terminator", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"list", "--", "--json"})

		if exitCode != ExitUsage {
			t.Fatalf("Run(list -- --json) exit code = %d, want %d", exitCode, ExitUsage)
		}
		if strings.HasPrefix(stderr.String(), "{") {
			t.Fatalf("Run(list -- --json) stderr = %q, want text diagnostic", stderr.String())
		}
	})

	t.Run("help usage error", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"help", "--json"})

		if exitCode != ExitUsage || stdout.Len() != 0 {
			t.Fatalf("Run(help --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
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
			t.Fatalf("JSON error envelope = %#v, want help usage failure", envelope)
		}
	})

	t.Run("unsupported result", func(t *testing.T) {
		t.Parallel()

		app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
			return Result{Value: func() {}}, nil
		})
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), []string{"list", "--json"})

		if exitCode != ExitOperational || stdout.Len() != 0 {
			t.Fatalf("Run(list --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
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
		if envelope.OK || envelope.Error.Code != "json_encoding_failed" {
			t.Fatalf("JSON error envelope = %#v, want encoding failure", envelope)
		}
	})
}

func TestRunnerPreservesApplicationExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		err            error
		want           int
		wantDiagnostic string
	}{
		{name: "operational fallback", err: errors.New("network unavailable"), want: ExitOperational, wantDiagnostic: "retry the command"},
		{name: "zero exit normalized", err: &Error{Message: "invalid success error"}, want: ExitOperational, wantDiagnostic: "retry the command"},
		{name: "changes required", err: &Error{ExitCode: ExitChanges, Code: "changes_required", Message: "project differs"}, want: ExitChanges, wantDiagnostic: "apply the reported changes"},
		{name: "conflict", err: &Error{ExitCode: ExitConflict, Code: "conflict", Message: "managed content changed"}, want: ExitConflict, wantDiagnostic: "resolve the conflicting managed content"},
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
			if test.wantDiagnostic != "" && !strings.Contains(stderr.String(), test.wantDiagnostic) {
				t.Fatalf("Run(check) stderr = %q, want %q", stderr.String(), test.wantDiagnostic)
			}
			if test.name == "operational fallback" && !strings.Contains(stderr.String(), "network unavailable") {
				t.Fatalf("Run(check) stderr = %q, want underlying error message", stderr.String())
			}
		})
	}
}

func TestRunnerRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		wantDiagnostic string
	}{
		{name: "unknown command", args: []string{"missing"}, wantDiagnostic: "acr help"},
		{name: "unknown help command", args: []string{"help", "missing"}, wantDiagnostic: "acr help"},
		{name: "unsupported flag", args: []string{"list", "--dry-run"}, wantDiagnostic: "acr list --help"},
		{name: "unsupported version flag", args: []string{"version", "--verbose"}, wantDiagnostic: "acr version --help"},
		{name: "missing uninstall source", args: []string{"uninstall"}},
		{name: "too many install sources", args: []string{"install", "one", "two"}},
		{name: "empty install source", args: []string{"install", ""}, wantDiagnostic: "omit SOURCE to reconcile"},
		{name: "empty install version", args: []string{"install", "github:owner/plugin@"}},
		{name: "install version with separator", args: []string{"install", "github:owner/plugin@release@candidate"}, wantDiagnostic: "must not contain @"},
		{name: "unsupported migration", args: []string{"migrate", "legacy"}},
		{name: "invalid freshness", args: []string{"init", "--freshness", "always"}},
		{name: "unsupported freshness subcommand", args: []string{"freshness", "check"}},
		{name: "invalid freshness policy", args: []string{"freshness", "run", "--policy", "always"}},
		{name: "empty project", args: []string{"check", "--project="}},
		{name: "empty separated project", args: []string{"check", "--project", ""}},
		{name: "empty separated agent", args: []string{"init", "--agent", ""}},
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
			if test.wantDiagnostic != "" && !strings.Contains(stderr.String(), test.wantDiagnostic) {
				t.Fatalf("Run(%v) stderr = %q, want %q", test.args, stderr.String(), test.wantDiagnostic)
			}
		})
	}
}

func TestRunnerRendersApplicationNotices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStdout string
		wantOK     bool
	}{
		{name: "text", args: []string{"freshness", "run"}},
		{name: "json", args: []string{"freshness", "run", "--json"}, wantStdout: `"notices":[{"code":"freshness_offline","message":"network unavailable"}]`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
				return Result{
					Value:    map[string]any{"policy": "outdated", "outdated": []any{}},
					Notices:  []Notice{{Code: "freshness_offline", Message: "network unavailable"}},
					ExitCode: ExitOperational,
				}, nil
			})
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), test.args)

			if exitCode != ExitOperational {
				t.Fatalf("Run(%v) exit code = %d, want %d", test.args, exitCode, ExitOperational)
			}
			if stderr.String() != "freshness_offline: network unavailable\n" {
				t.Fatalf("Run(%v) stderr = %q", test.args, stderr.String())
			}
			if test.wantStdout == "" && stdout.Len() != 0 {
				t.Fatalf("Run(%v) stdout = %q, want empty", test.args, stdout.String())
			}
			if test.wantStdout != "" {
				var envelope struct {
					OK bool `json:"ok"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
					t.Fatalf("decode JSON stdout %q: %v", stdout.String(), err)
				}
				if envelope.OK != test.wantOK {
					t.Fatalf("Run(%v) ok = %t, want %t", test.args, envelope.OK, test.wantOK)
				}
				if !strings.Contains(stdout.String(), test.wantStdout) {
					t.Fatalf("Run(%v) stdout = %q, want %q", test.args, stdout.String(), test.wantStdout)
				}
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

	t.Run("flag terminator", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "1.2.3").Run(context.Background(), []string{"version", "--", "--json"})
		if exitCode != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: acr version") {
			t.Fatalf("Run(version -- --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})

	t.Run("trailing flag terminator", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "1.2.3").Run(context.Background(), []string{"version", "--"})
		if exitCode != ExitSuccess || strings.TrimSpace(stdout.String()) != "1.2.3" || stderr.Len() != 0 {
			t.Fatalf("Run(version --) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})
}

func TestRunnerReturnsOperationalExitWhenTextOutputFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		app  Application
	}{
		{name: "root help", app: rejectingApplication(t)},
		{name: "help command", args: []string{"help"}, app: rejectingApplication(t)},
		{name: "command help", args: []string{"list", "--help"}, app: rejectingApplication(t)},
		{name: "version help", args: []string{"version", "--help"}, app: rejectingApplication(t)},
		{name: "version", args: []string{"version"}, app: rejectingApplication(t)},
		{
			name: "application result",
			args: []string{"list"},
			app: ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
				return Result{Message: "ok"}, nil
			}),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			exitCode := New(failingWriter{}, &stderr, test.app, "test").Run(context.Background(), test.args)

			if exitCode != ExitOperational {
				t.Fatalf("Run(%v) exit code = %d, want %d", test.args, exitCode, ExitOperational)
			}
			if !strings.Contains(stderr.String(), "verify stdout is writable and retry") {
				t.Fatalf("Run(%v) stderr = %q, want actionable output diagnostic", test.args, stderr.String())
			}
		})
	}
}

func TestRunnerReturnsOperationalExitWhenTextDiagnosticFails(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	exitCode := New(&stdout, failingWriter{}, rejectingApplication(t), "test").Run(context.Background(), []string{"missing"})

	if exitCode != ExitOperational {
		t.Fatalf("Run(missing) exit code = %d, want %d", exitCode, ExitOperational)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run(missing) stdout = %q, want empty", stdout.String())
	}
}

func TestRunnerReturnsOperationalExitWhenJSONOutputFails(t *testing.T) {
	t.Parallel()

	app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
		return Result{Value: map[string]bool{"ok": true}}, nil
	})
	var stderr bytes.Buffer
	exitCode := New(failingWriter{}, &stderr, app, "test").Run(context.Background(), []string{"list", "--json"})

	if exitCode != ExitOperational {
		t.Fatalf("Run(list --json) exit code = %d, want %d", exitCode, ExitOperational)
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
	if envelope.OK || envelope.Error.Code != "output_failed" {
		t.Fatalf("JSON error envelope = %#v, want output failure", envelope)
	}
}

func TestRunnerCompletesShortJSONWrites(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		app := ApplicationFunc(func(_ context.Context, _ Invocation) (Result, error) {
			return Result{Value: map[string]bool{"accepted": true}}, nil
		})
		stdout := &shortWriter{}
		var stderr bytes.Buffer
		exitCode := New(stdout, &stderr, app, "test").Run(context.Background(), []string{"list", "--json"})

		if exitCode != ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("Run(list --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
		var envelope struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK {
			t.Fatalf("decode JSON stdout %q: envelope = %#v, err = %v", stdout.String(), envelope, err)
		}
	})

	t.Run("diagnostic", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		stderr := &shortWriter{}
		exitCode := New(&stdout, stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"missing", "--json"})

		if exitCode != ExitUsage || stdout.Len() != 0 {
			t.Fatalf("Run(missing --json) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
		var envelope struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil || envelope.OK {
			t.Fatalf("decode JSON stderr %q: envelope = %#v, err = %v", stderr.String(), envelope, err)
		}
	})
}

func TestRunnerCompletesShortTextWrites(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		stdout := &shortWriter{}
		var stderr bytes.Buffer
		exitCode := New(stdout, &stderr, rejectingApplication(t), "1.2.3").Run(context.Background(), []string{"version"})

		if exitCode != ExitSuccess || stdout.String() != "1.2.3\n" || stderr.Len() != 0 {
			t.Fatalf("Run(version) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})

	t.Run("diagnostic", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		stderr := &shortWriter{}
		exitCode := New(&stdout, stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"missing"})

		if exitCode != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "acr help") {
			t.Fatalf("Run(missing) exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type shortWriter struct {
	bytes.Buffer
}

func (writer *shortWriter) Write(data []byte) (int, error) {
	length := len(data) / 2
	if length == 0 {
		length = 1
	}
	return writer.Buffer.Write(data[:length])
}

func rejectingApplication(t *testing.T) Application {
	t.Helper()
	return ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
		t.Fatalf("application unexpectedly received invocation %#v", invocation)
		return Result{}, nil
	})
}
