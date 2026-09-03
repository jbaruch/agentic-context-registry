package setupapp

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

type hostilePromptApplication struct {
	calls        []cli.Invocation
	networkCalls int
}

func (application *hostilePromptApplication) Execute(_ context.Context, invocation cli.Invocation) (cli.Result, error) {
	application.calls = append(application.calls, invocation)
	if len(application.calls) == 1 {
		return cli.Result{}, downgradeRequired()
	}
	return cli.Result{Message: "choice accepted", Value: map[string]string{"choice": string(invocation.Downgrade)}}, nil
}

func TestHostileDowngradePromptMatrixLeavesTheTreeUntouchedAndUsesNoNetwork(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantExit    int
		wantChoice  cli.DowngradeChoice
		wantCalls   int
		wantError   string
		wantRetries int
	}{
		{name: "hold", input: "hold\n", wantChoice: cli.DowngradeHold, wantCalls: 2},
		{name: "pin", input: "pin\n", wantChoice: cli.DowngradePin, wantCalls: 2},
		{name: "cancel", input: "cancel\n", wantExit: cli.ExitUsage, wantCalls: 1, wantError: "downgrade_cancelled"},
		{name: "empty", input: "\n", wantExit: cli.ExitUsage, wantCalls: 1, wantError: "downgrade_cancelled"},
		{name: "three garbage answers", input: "garbage\nstill-garbage\nnope\n", wantExit: cli.ExitUsage, wantCalls: 1, wantError: "downgrade_cancelled", wantRetries: 3},
		{name: "EOF", input: "", wantExit: cli.ExitUsage, wantCalls: 1, wantError: "downgrade_cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := heldProject(t)
			before := hashTree(t, root)
			inner := &hostilePromptApplication{}
			var stdout, stderr bytes.Buffer
			application := NewApplication(inner, NewTerminalPrompter(strings.NewReader(test.input), &stderr, true))

			exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), []string{
				"install", heldSource + "@v1.0.0", "--project", root,
			})

			if exitCode != test.wantExit {
				t.Fatalf("exit = %d, want %d; stdout = %q stderr = %q", exitCode, test.wantExit, stdout.String(), stderr.String())
			}
			if len(inner.calls) != test.wantCalls {
				t.Fatalf("inner calls = %d, want %d", len(inner.calls), test.wantCalls)
			}
			if test.wantCalls == 2 && inner.calls[1].Downgrade != test.wantChoice {
				t.Fatalf("second invocation choice = %q, want %q", inner.calls[1].Downgrade, test.wantChoice)
			}
			if test.wantError != "" && !strings.Contains(stderr.String(), "rollback cancelled") {
				t.Fatalf("stderr = %q, want cancellation diagnostic for %q", stderr.String(), test.wantError)
			}
			if strings.Contains(stdout.String(), "Installing "+heldSource) {
				t.Fatalf("prompt leaked to stdout: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Record it as:") {
				t.Fatalf("prompt was not written to stderr: %q", stderr.String())
			}
			if test.wantRetries != 0 && strings.Count(stderr.String(), "Record it as:") != test.wantRetries {
				t.Fatalf("prompt count = %d, want %d", strings.Count(stderr.String(), "Record it as:"), test.wantRetries)
			}
			if inner.networkCalls != 0 {
				t.Fatalf("prompt path made %d network call(s)", inner.networkCalls)
			}
			if after := hashTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("prompt path changed the project:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

func TestHostileJSONAndNonInteractiveDowngradesNeverConstructAQuestion(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "json", args: []string{"--json"}},
		{name: "non-interactive", args: []string{"--non-interactive"}},
		{name: "json and non-interactive", args: []string{"--json", "--non-interactive"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := heldProject(t)
			before := hashTree(t, root)
			inner := &hostilePromptApplication{}
			reader := &countingReader{}
			var stdout, stderr bytes.Buffer
			application := NewApplication(inner, NewTerminalPrompter(reader, &stderr, true))
			args := append([]string{"install", heldSource + "@v1.0.0", "--project", root}, test.args...)

			exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), args)

			if exitCode != cli.ExitUsage || stdout.Len() != 0 || reader.reads != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q, reads = %d", exitCode, stdout.String(), stderr.String(), reader.reads)
			}
			if strings.Contains(stderr.String(), "Record it as:") || !strings.Contains(stderr.String(), "--hold") || !strings.Contains(stderr.String(), "--pin") {
				t.Fatalf("suppressed prompt stderr = %q", stderr.String())
			}
			if len(inner.calls) != 1 || inner.networkCalls != 0 {
				t.Fatalf("inner calls = %d, network calls = %d", len(inner.calls), inner.networkCalls)
			}
			if strings.Contains(test.name, "json") {
				if strings.Count(stderr.String(), "\n") != 1 || !strings.HasPrefix(stderr.String(), "{") || !strings.Contains(stderr.String(), "downgrade_choice_required") {
					t.Fatalf("JSON stderr is not exactly one envelope: %q", stderr.String())
				}
			}
			if after := hashTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("suppressed prompt changed the tree:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

func TestHostileInitSelectionMatrix(t *testing.T) {
	t.Run("fresh detected selection is preselected", func(t *testing.T) {
		root := t.TempDir()
		prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
			"agents": {Values: []string{"codex"}}, "freshness": {Values: []string{"outdated"}},
		}}
		_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{detected: []string{"codex"}}), "init", "--project", root)
		if exitCode != cli.ExitSuccess || stderr != "" {
			t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
		}
		question, ok := prompter.question("agents")
		if !ok || !reflect.DeepEqual(selectedOptions(question), []string{"codex"}) {
			t.Fatalf("agent question = %#v", question)
		}
	})

	t.Run("stored selection wins while detected candidate stays unselected", func(t *testing.T) {
		root := storedProject(t)
		before := hashTree(t, root)
		prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
		stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{detected: []string{"claude-code"}}), "init", "--project", root, "--json")
		if exitCode != cli.ExitSuccess || stderr != "" || strings.Count(stdout, "\n") != 1 || !strings.Contains(stdout, `"changed":false`) {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
		}
		if len(prompter.asked) != 0 {
			t.Fatalf("JSON init asked questions: %#v", prompter.asked)
		}
		if after := hashTree(t, root); !reflect.DeepEqual(after, before) {
			t.Fatalf("stored JSON init changed the tree")
		}

		prompter = &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
		_, stderr, exitCode = runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{detected: []string{"claude-code"}}), "init", "--project", root)
		if exitCode != cli.ExitSuccess || stderr != "" {
			t.Fatalf("interactive stored init exit = %d, stderr = %q", exitCode, stderr)
		}
		question, ok := prompter.question("agents")
		if !ok || !reflect.DeepEqual(selectedOptions(question), []string{"codex"}) {
			t.Fatalf("stored preselection = %#v", question)
		}
		for _, option := range question.Options {
			if option.Value == "claude-code" && (option.Selected || !strings.Contains(option.Label, "detected")) {
				t.Fatalf("detected but unstored option = %#v", option)
			}
		}
	})

	t.Run("agent flag deduplicates and replaces stored selection", func(t *testing.T) {
		root := storedProject(t)
		prompter := &recordingPrompter{interactive: true}
		detector := &detectorCounter{detected: []string{"claude-code"}}
		_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detector),
			"init", "--project", root, "--agent", "cursor", "--agent", "cursor")
		if exitCode != cli.ExitSuccess || stderr != "" || detector.calls != 0 || len(prompter.asked) != 0 {
			t.Fatalf("exit = %d, stderr = %q, detect calls = %d, questions = %#v", exitCode, stderr, detector.calls, prompter.asked)
		}
		if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"cursor"}) {
			t.Fatalf("stored agents = %#v", got)
		}
	})

	t.Run("non-interactive fresh stored and empty detection", func(t *testing.T) {
		tests := []struct {
			name     string
			root     func(*testing.T) string
			detected []string
			want     []string
			wantExit int
		}{
			{name: "fresh", root: func(t *testing.T) string { return t.TempDir() }, detected: []string{"codex"}, want: []string{"codex"}},
			{name: "stored", root: func(t *testing.T) string { return storedProject(t) }, detected: []string{"claude-code"}, want: []string{"codex"}},
			{name: "empty", root: func(t *testing.T) string { return t.TempDir() }, wantExit: cli.ExitUsage},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				root := test.root(t)
				prompter := &recordingPrompter{interactive: true}
				stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{detected: test.detected}),
					"init", "--project", root, "--non-interactive", "--json")
				if exitCode != test.wantExit || len(prompter.asked) != 0 {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q, questions = %#v", exitCode, stdout, stderr, prompter.asked)
				}
				if test.wantExit == cli.ExitUsage {
					if stdout != "" || !strings.Contains(stderr, `"code":"no_agent_selected"`) || !strings.Contains(stderr, "--agent") {
						t.Fatalf("empty refusal stdout = %q, stderr = %q", stdout, stderr)
					}
					return
				}
				if stderr != "" || !reflect.DeepEqual(loadedProject(t, root).Agents, test.want) {
					t.Fatalf("stdout = %q, stderr = %q, project = %#v", stdout, stderr, loadedProject(t, root))
				}
			})
		}
	})
}

func TestHostileFirstInstallPromptsOnlyWhenAgentsYAMLIsMissing(t *testing.T) {
	tests := []struct {
		name       string
		root       func(*testing.T) string
		wantPrompt bool
	}{
		{name: "missing", root: func(t *testing.T) string { return t.TempDir() }, wantPrompt: true},
		{name: "present empty", root: func(t *testing.T) string { return configuredProject(t, "schemaVersion: 2\nagents: []\n") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := test.root(t)
			prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
				"agents": {Values: []string{"codex"}}, "freshness": {Values: []string{"outdated"}},
			}}
			inner := &scriptedApplication{outcomes: []scriptedOutcome{{result: cli.Result{Message: "installed", Value: map[string]bool{"changed": false}}}}}
			stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(inner, prompter, &detectorCounter{detected: []string{"codex"}}),
				"install", "github:owner/plugin", "--project", root)
			if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "installed") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			_, prompted := prompter.question("agents")
			if prompted != test.wantPrompt {
				t.Fatalf("agent prompt = %t, want %t", prompted, test.wantPrompt)
			}
		})
	}

	t.Run("JSON emits one envelope and no prompt", func(t *testing.T) {
		root := t.TempDir()
		prompter := &recordingPrompter{interactive: true}
		inner := &scriptedApplication{outcomes: []scriptedOutcome{{result: cli.Result{Value: map[string]bool{"changed": false}}}}}
		stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(inner, prompter, &detectorCounter{detected: []string{"codex"}}),
			"install", "github:owner/plugin", "--project", root, "--json")
		if exitCode != cli.ExitSuccess || stderr != "" || strings.Count(stdout, "\n") != 1 || !strings.HasPrefix(stdout, "{") {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
		}
		if len(prompter.asked) != 0 || strings.Contains(stdout, "Which agents") || strings.Contains(stdout, "session start") {
			t.Fatalf("JSON install prompted: stdout = %q, questions = %#v", stdout, prompter.asked)
		}
	})
}

func TestHostilePromptErrorsRemainTyped(t *testing.T) {
	root := heldProject(t)
	inner := &hostilePromptApplication{}
	application := NewApplication(inner, NewTerminalPrompter(strings.NewReader(""), &bytes.Buffer{}, true))
	_, err := application.Execute(context.Background(), installInvocation(root))
	var commandError *cli.Error
	if !errors.As(err, &commandError) || commandError.Code != "downgrade_cancelled" {
		t.Fatalf("Execute() error = %v", err)
	}
}
