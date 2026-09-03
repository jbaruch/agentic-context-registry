package setupapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestTesterReverifyInitEndOfInputWritesNoState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "EOF on agent question", input: ""},
		{name: "EOF on freshness question", input: "codex\n"},
		{name: "partial agent line without newline", input: "codex"},
		{name: "partial freshness line without newline", input: "codex\noutdated"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			sentinel := filepath.Join(root, ".operator-state")
			if err := os.WriteFile(sentinel, []byte("unchanged\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := hashTree(t, root)
			var stdout, stderr bytes.Buffer
			application := NewApplication(nil, NewTerminalPrompter(bytes.NewBufferString(test.input), &stderr, true))
			application.detect = func(context.Context, string) ([]string, error) { return []string{"codex"}, nil }

			exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(
				context.Background(), []string{"init", "--project", root})

			if exitCode != cli.ExitUsage || stdout.Len() != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
			}
			if after := hashTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("cancelled init changed the path/mode/SHA-256 tree:\n before %#v\n after  %#v", before, after)
			}
			for _, statePath := range []string{dependency.ProjectFilename, dependency.LockFilename} {
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(statePath))); !os.IsNotExist(err) {
					t.Fatalf("cancelled init wrote %s: %v", statePath, err)
				}
			}
		})
	}
}

func TestTesterReverifyConfiguredNonInteractiveInitNeverDetects(t *testing.T) {
	t.Parallel()

	root := storedProject(t)
	before := hashTree(t, root)
	prompter := &recordingPrompter{interactive: true}
	application := NewApplication(nil, prompter)
	application.detect = func(context.Context, string) ([]string, error) {
		panic("configured non-interactive init called the detector")
	}

	stdout, stderr, exitCode := runSetupCLI(t, application, "init", "--project", root, "--non-interactive")

	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("init exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if got := loadedProject(t, root).Agents; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("stored selection = %#v, want codex", got)
	}
	if len(prompter.asked) != 0 {
		t.Fatalf("non-interactive init asked questions: %#v", prompter.asked)
	}
	if after := hashTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("configured non-interactive init changed the tree:\n before %#v\n after  %#v", before, after)
	}
}

func TestTesterReverifySetupPackageContainsNoRuntimeAgentsState(t *testing.T) {
	t.Parallel()

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(workingDirectory, dependency.ProjectFilename)
	if _, err := os.Lstat(leaked); !os.IsNotExist(err) {
		t.Fatalf("runtime state remains in the setupapp source tree at %s: %v", leaked, err)
	}
}
