package setupapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// recordingPrompter answers from a script and keeps every question it was
// asked, so a test asserts what was pre-selected rather than what was typed.
type recordingPrompter struct {
	interactive bool
	answers     map[string]Answer
	asked       []Question
}

func (prompter *recordingPrompter) Interactive() bool {
	return prompter.interactive
}

func (prompter *recordingPrompter) Ask(_ context.Context, question Question) (Answer, error) {
	prompter.asked = append(prompter.asked, question)
	answer, scripted := prompter.answers[question.ID]
	if !scripted {
		return Answer{Cancelled: true}, nil
	}
	return answer, nil
}

func (prompter *recordingPrompter) question(id string) (Question, bool) {
	for _, question := range prompter.asked {
		if question.ID == id {
			return question, true
		}
	}
	return Question{}, false
}

type detectorCounter struct {
	detected []string
	calls    int
}

func setupApplicationFor(inner cli.Application, prompter Prompter, detected *detectorCounter) *Application {
	application := NewApplication(inner, prompter)
	application.detect = func(context.Context, string) ([]string, error) {
		detected.calls++
		return append([]string(nil), detected.detected...), nil
	}
	return application
}

func runSetupCLI(t *testing.T, application cli.Application, args ...string) (string, string, int) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), args)
	return stdout.String(), stderr.String(), exitCode
}

func selectedOptions(question Question) []string {
	var selected []string
	for _, option := range question.Options {
		if option.Selected {
			selected = append(selected, option.Value)
		}
	}
	return selected
}

// configuredProject writes an agents.yaml by hand so the test asserts against
// exactly the bytes a project could already have, unknown keys included.
func configuredProject(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, dependency.ProjectFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func storedProject(t *testing.T) string {
	t.Helper()
	return configuredProject(t, "schemaVersion: 2\nagents:\n  - codex\nfreshness: outdated\n")
}

func loadedProject(t *testing.T, root string) dependency.Project {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Project
}

func TestInitPreselectsDetectedAgentsOnAFreshProject(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
		"agents":    {Values: []string{"codex"}},
		"freshness": {Values: []string{"outdated"}},
	}}
	detected := &detectorCounter{detected: []string{"codex"}}
	root := t.TempDir()

	stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", root)

	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Selected codex") {
		t.Fatalf("init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	question, asked := prompter.question("agents")
	if !asked {
		t.Fatal("init did not ask the agent question")
	}
	if !reflect.DeepEqual(selectedOptions(question), []string{"codex"}) {
		t.Fatalf("pre-selected agents = %#v, want the detected set", selectedOptions(question))
	}
	if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("written agents = %#v", got)
	}
}

func TestInitPreselectsStoredAgentsOverDetection(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
	detected := &detectorCounter{detected: []string{"claude-code", "codex"}}
	root := storedProject(t)

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", root)

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("init exit = %d, stderr = %q", exitCode, stderr)
	}
	question, _ := prompter.question("agents")
	if !reflect.DeepEqual(selectedOptions(question), []string{"codex"}) {
		t.Fatalf("pre-selected agents = %#v, want the stored selection", selectedOptions(question))
	}
}

func TestInitOffersDetectedButUnstoredAgentsUnselected(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
	detected := &detectorCounter{detected: []string{"claude-code", "codex"}}

	_, _, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", storedProject(t))

	if exitCode != cli.ExitSuccess {
		t.Fatalf("init exit = %d", exitCode)
	}
	question, _ := prompter.question("agents")
	for _, option := range question.Options {
		if option.Value != "claude-code" {
			continue
		}
		if option.Selected {
			t.Fatal("a detected but unstored agent was pre-selected")
		}
		if !strings.Contains(option.Label, "detected") {
			t.Fatalf("detected candidate label = %q, want it marked as detected", option.Label)
		}
		return
	}
	t.Fatalf("claude-code was not offered as a candidate: %#v", question.Options)
}

func TestInitKeepsStoredAgentsThatDetectionMisses(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
	detected := &detectorCounter{}
	root := storedProject(t)

	_, _, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", root)

	if exitCode != cli.ExitSuccess {
		t.Fatalf("init exit = %d", exitCode)
	}
	question, _ := prompter.question("agents")
	if !reflect.DeepEqual(selectedOptions(question), []string{"codex"}) {
		t.Fatalf("pre-selected agents = %#v, want the stored selection detection missed", selectedOptions(question))
	}
	if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("written agents = %#v", got)
	}
}

func TestInitAgentFlagSkipsDetectionAndPrompt(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true}
	detected := &detectorCounter{detected: []string{"claude-code"}}
	root := storedProject(t)

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected),
		"init", "--project", root, "--agent", "cursor", "--agent", "cursor")

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("init exit = %d, stderr = %q", exitCode, stderr)
	}
	if detected.calls != 0 || len(prompter.asked) != 0 {
		t.Fatalf("--agent ran detection %d time(s) and asked %d question(s)", detected.calls, len(prompter.asked))
	}
	if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("written agents = %#v, want the flag to replace the stored selection", got)
	}
}

func TestInitNonInteractiveTakesDetectedSetOnAFreshProject(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true}
	detected := &detectorCounter{detected: []string{"codex"}}
	root := t.TempDir()

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", root, "--non-interactive")

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("init exit = %d, stderr = %q", exitCode, stderr)
	}
	if len(prompter.asked) != 0 {
		t.Fatalf("--non-interactive asked %d question(s)", len(prompter.asked))
	}
	project := loadedProject(t, root)
	if !reflect.DeepEqual(project.Agents, []string{"codex"}) || project.Freshness != "outdated" {
		t.Fatalf("written project = %#v", project)
	}
}

func TestInitNonInteractiveKeepsTheStoredSelection(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true}
	detected := &detectorCounter{detected: []string{"claude-code", "cursor"}}
	root := storedProject(t)

	stdout, _, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, detected), "init", "--project", root, "--non-interactive")

	if exitCode != cli.ExitSuccess || !strings.Contains(stdout, "already selects codex") {
		t.Fatalf("init exit = %d, stdout = %q", exitCode, stdout)
	}
	if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("stored agents = %#v, want detection to contribute nothing", got)
	}
}

func TestInitNonInteractiveWithoutDetectionRefuses(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true}
	root := t.TempDir()

	stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{}),
		"init", "--project", root, "--non-interactive", "--json")

	if exitCode != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, `"code":"no_agent_selected"`) {
		t.Fatalf("init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "--agent") {
		t.Fatalf("refusal = %q, want --agent named", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, dependency.ProjectFilename)); !os.IsNotExist(err) {
		t.Fatalf("refused init wrote %s: %v", dependency.ProjectFilename, err)
	}
}

func TestInitFreshnessPromptDefaultsToOutdated(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input string
		args  []string
	}{
		"empty answers":   {input: "\n\n"},
		"non-interactive": {input: "", args: []string{"--non-interactive"}},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			var questions bytes.Buffer
			application := NewApplication(nil, NewTerminalPrompter(strings.NewReader(test.input), &questions, true))
			application.detect = func(context.Context, string) ([]string, error) { return []string{"codex"}, nil }
			args := append([]string{"init", "--project", root}, test.args...)

			_, stderr, exitCode := runSetupCLI(t, application, args...)

			if exitCode != cli.ExitSuccess {
				t.Fatalf("%s init exit = %d, stderr = %q", name, exitCode, stderr)
			}
			if got := loadedProject(t, root).Freshness; got != "outdated" {
				t.Fatalf("%s freshness = %q, want outdated", name, got)
			}
		})
	}
}

func TestInitIsIdempotent(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{"agents": {Values: []string{"codex"}}}}
	root := storedProject(t)
	application := setupApplicationFor(nil, prompter, &detectorCounter{detected: []string{"codex"}})
	before := hashTree(t, root)

	stdout, stderr, exitCode := runSetupCLI(t, application, "init", "--project", root, "--json")

	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"changed":false`) {
		t.Fatalf("init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("idempotent init wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestInitDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
		"agents":    {Values: []string{"cursor"}},
		"freshness": {Values: []string{"install"}},
	}}
	root := storedProject(t)
	before := hashTree(t, root)

	stdout, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{}), "init", "--project", root, "--dry-run")

	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, "Would select cursor") {
		t.Fatalf("dry-run init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry-run init wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestInitPreservesUnknownProjectFields(t *testing.T) {
	t.Parallel()

	root := configuredProject(t, "schemaVersion: 2\nagents:\n  - codex\nexperimental:\n  owner: someone\n")
	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
		"agents":    {Values: []string{"cursor"}},
		"freshness": {Values: []string{"install"}},
	}}

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(nil, prompter, &detectorCounter{}), "init", "--project", root)

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("init exit = %d, stderr = %q", exitCode, stderr)
	}
	written, err := os.ReadFile(filepath.Join(root, dependency.ProjectFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "owner: someone") {
		t.Fatalf("init dropped an unknown top-level field: %s", written)
	}
	if !strings.Contains(string(written), "cursor") {
		t.Fatalf("init did not write the answered selection: %s", written)
	}
}

func TestFirstInstallWithoutAgentsYamlRunsInitQuestions(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true, answers: map[string]Answer{
		"agents":    {Values: []string{"codex"}},
		"freshness": {Values: []string{"none"}},
	}}
	inner := &scriptedApplication{outcomes: []scriptedOutcome{{result: cli.Result{Message: "installed"}}}}
	root := t.TempDir()

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(inner, prompter, &detectorCounter{detected: []string{"codex"}}),
		"install", "github:owner/plugin", "--project", root)

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("first install exit = %d, stderr = %q", exitCode, stderr)
	}
	if _, asked := prompter.question("agents"); !asked {
		t.Fatalf("first install asked %#v, want the agent question", prompter.asked)
	}
	project := loadedProject(t, root)
	if !reflect.DeepEqual(project.Agents, []string{"codex"}) || project.Freshness != "none" {
		t.Fatalf("first install wrote %#v", project)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("inner application called %d time(s), want 1", len(inner.calls))
	}
}

func TestFirstInstallDoesNotRepromptWhenAgentsListIsEmpty(t *testing.T) {
	t.Parallel()

	prompter := &recordingPrompter{interactive: true}
	inner := &scriptedApplication{outcomes: []scriptedOutcome{{result: cli.Result{Message: "installed"}}}}
	root := configuredProject(t, "schemaVersion: 2\nagents: []\n")

	_, stderr, exitCode := runSetupCLI(t, setupApplicationFor(inner, prompter, &detectorCounter{detected: []string{"codex"}}),
		"install", "github:owner/plugin", "--project", root)

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("install exit = %d, stderr = %q", exitCode, stderr)
	}
	if len(prompter.asked) != 0 {
		t.Fatalf("an existing agents.yaml with an empty selection was re-asked: %#v", prompter.asked)
	}
}
