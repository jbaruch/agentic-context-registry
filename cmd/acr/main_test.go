package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"version"})

	if exitCode != 0 {
		t.Fatalf("run(version) exit code = %d, want 0", exitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Fatalf("run(version) stdout = %q, want %q", got, version)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"help"})

	if exitCode != 0 {
		t.Fatalf("run(help) exit code = %d, want 0", exitCode)
	}
	if got := stdout.String(); !strings.Contains(got, "usage: acr") {
		t.Fatalf("run(help) stdout = %q, want usage", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(help) stderr = %q, want empty", stderr.String())
	}
}

func TestRunWithoutCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, nil)

	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}
	if got := stdout.String(); !strings.Contains(got, "Agentic Context Registry") {
		t.Fatalf("run() stdout = %q, want product name", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(&stdout, &stderr, []string{"missing"})

	if exitCode != 2 {
		t.Fatalf("run(missing) exit code = %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(missing) stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "unknown command") {
		t.Fatalf("run(missing) stderr = %q, want unknown-command diagnostic", got)
	}
}
