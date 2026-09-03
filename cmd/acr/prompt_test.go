package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPrompterIsNonInteractiveOnAPipeAndOnAClosedStdin(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	regular, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { regular.Close() })

	tests := map[string]*os.File{"pipe": reader, "regular file": regular, "closed descriptor": closed}
	for name, stdin := range tests {
		name, stdin := name, stdin
		t.Run(name, func(t *testing.T) {
			if interactiveStdin(stdin) {
				t.Fatalf("interactiveStdin(%s) = true, want false", name)
			}
		})
	}
	t.Run("not a file", func(t *testing.T) {
		if interactiveStdin(strings.NewReader("")) {
			t.Fatal("interactiveStdin(non-file reader) = true, want false")
		}
	})
}

// TestNonTerminalStdinRefusesThroughTheComposedApplication drives run(), the
// real wiring of the terminal prompter around the shipped application, rather
// than the probe alone. Each non-terminal stdin shape has to reach the typed
// refusal the inner application already returns, leave stdout carrying nothing
// but program output, and never read the stream a caller happened to supply.
func TestNonTerminalStdinRefusesThroughTheComposedApplication(t *testing.T) {
	tests := map[string]struct {
		args     []string
		wantCode bool
	}{
		"pipe":            {},
		"closed stdin":    {},
		"json":            {args: []string{"--json"}, wantCode: true},
		"non-interactive": {args: []string{"--non-interactive", "--json"}, wantCode: true},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			stdin, pending, wantPending := pendingStdin(t, name == "closed stdin")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			args := append([]string{"init", "--project", t.TempDir()}, test.args...)

			exitCode := run(stdin, &stdout, &stderr, args)

			if exitCode != 2 {
				t.Fatalf("%s init exit = %d, want 2; stderr = %q", name, exitCode, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s init wrote %q to stdout, want nothing", name, stdout.String())
			}
			if !strings.Contains(stderr.String(), "--agent") {
				t.Fatalf("%s refusal = %q, want the remedy flag named", name, stderr.String())
			}
			if test.wantCode && !strings.Contains(stderr.String(), `"code":"no_agent_selected"`) {
				t.Fatalf("%s refusal = %q, want the typed code", name, stderr.String())
			}
			for _, question := range []string{"Which agents", "What should ACR do", "Answer with"} {
				if strings.Contains(stdout.String()+stderr.String(), question) {
					t.Fatalf("%s init asked %q instead of refusing", name, question)
				}
			}
			if unread := pending(); unread != wantPending {
				t.Fatalf("%s init read stdin: %q remains, want %q", name, unread, wantPending)
			}
		})
	}
}

// pendingStdin returns a non-terminal stdin, a reader for whatever is still
// buffered in it, and what that buffer must still hold. The pipe carries an
// answer no question asked for, so a prompter that consumed it would leave the
// pipe empty and fail the caller's comparison.
func pendingStdin(t *testing.T, closed bool) (stdin *os.File, unread func() string, want string) {
	t.Helper()
	if closed {
		descriptor, err := os.CreateTemp(t.TempDir(), "closed")
		if err != nil {
			t.Fatal(err)
		}
		if err := descriptor.Close(); err != nil {
			t.Fatal(err)
		}
		return descriptor, func() string { return "" }, ""
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "codex\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return reader, func() string {
		t.Helper()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		remaining, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(remaining)
	}, "codex\n"
}
